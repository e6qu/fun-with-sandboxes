package shim

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
	"github.com/e6qu/fun-with-sandboxes/internal/pathmap"
	"github.com/e6qu/fun-with-sandboxes/internal/roles"
	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
)

// workBinary is the test program the shim runs, and library the container
// library every container process loads.
var workBinary, library string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "shimwork")
	if err != nil {
		panic(err)
	}
	workBinary = filepath.Join(dir, "work")
	if out, err := exec.Command("go", "build", "-o", workBinary, "./testdata/work").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build work: %v\n%s", err, out))
	}
	if library, err = pathmap.Build(filepath.Join(dir, "lib")); err != nil {
		panic(fmt.Sprintf("build the container library: %v", err))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type harness struct {
	t      *testing.T
	cfg    Config
	dir    string
	client *Client
	ready  Ready
}

// launch runs the work program with args under a shim.
func launch(t *testing.T, args []string, mod func(*Config)) *harness {
	t.Helper()
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(state, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(workBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "work"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	tag := "SANDBOX_ROOT=" + root
	// macOS caps socket paths at 104 bytes, more than a test's temporary
	// directory leaves.
	sockDir, err := os.MkdirTemp("/tmp", "shim")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	cfg := Config{
		ID: "test",
		Spec: runtime.Spec{
			Root: root, Argv: append([]string{"/work"}, args...), Env: []string{"PATH=/", tag},
			Profile:     seatbelt.Spec{Root: root, StateRoot: state, IPCPrefix: seatbelt.IPCPrefixFor(root), Libraries: []string{library}},
			ProfilePath: filepath.Join(state, "profile.sb"), Library: library,
		},
		LogPath: filepath.Join(state, "log.jsonl"), Socket: filepath.Join(sockDir, "s.sock"),
		ExitPath: filepath.Join(state, "exit.json"), Tag: tag,
	}
	if mod != nil {
		mod(&cfg)
	}
	c, ready, err := Launch(state, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, cfg: cfg, dir: state, client: c, ready: ready}
	t.Cleanup(func() {
		c.Signal(syscall.SIGKILL)
		c.Wait(nil)
	})
	return h
}

func (h *harness) logs() string {
	f, err := os.Open(h.cfg.LogPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	var b strings.Builder
	s := bufio.NewScanner(f)
	for s.Scan() {
		var e struct{ Log string }
		json.Unmarshal(s.Bytes(), &e)
		b.WriteString(e.Log)
	}
	return b.String()
}

// waitLog waits for the container to log want, woken by the shim's log
// notifications.
func (h *harness) waitLog(want string) {
	h.t.Helper()
	woke := make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	go h.client.Logs(stop, func() {
		select {
		case woke <- struct{}{}:
		default:
		}
	})
	deadline := time.After(30 * time.Second)
	for !strings.Contains(h.logs(), want) {
		select {
		case <-woke:
		case <-deadline:
			h.t.Fatalf("the container did not log %q; it logged %q", want, h.logs())
		}
	}
}

func (h *harness) wait() Exit {
	h.t.Helper()
	got := make(chan Exit, 1)
	go func() {
		e, err := h.client.Wait(nil)
		if err != nil {
			// A container that ended before Wait dialled took its shim's
			// socket with it; the shim recorded the exit, as the engine
			// reads it.
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
				r, rerr := ReadExit(h.cfg.ExitPath)
				if rerr == nil {
					e = r
					break
				}
				if time.Now().After(deadline) {
					h.t.Errorf("%v, and no exit recorded: %v", err, rerr)
					break
				}
			}
		}
		got <- e
	}()
	select {
	case e := <-got:
		return e
	case <-time.After(30 * time.Second):
		h.t.Fatal("the container did not exit")
	}
	return Exit{}
}

func TestOutputAndExit(t *testing.T) {
	h := launch(t, []string{"echo", "one", "two"}, nil)
	e := h.wait()
	if e.ExitCode != 3 || e.OOMKilled || e.FinishedAt.IsZero() {
		t.Fatalf("exit %+v, want status 3", e)
	}
	if got := h.logs(); got != "one\ntwo\n" {
		t.Fatalf("logs %q", got)
	}
	recorded, err := ReadExit(h.cfg.ExitPath)
	if err != nil || recorded.ExitCode != 3 {
		t.Fatalf("recorded exit %+v, %v", recorded, err)
	}
	if _, err := os.Stat(h.cfg.Socket); !os.IsNotExist(err) {
		t.Fatalf("the shim left its socket behind: %v", err)
	}
}

func TestStdin(t *testing.T) {
	h := launch(t, []string{"cat"}, func(c *Config) { c.OpenStdin = true })
	w, err := h.client.Stdin(true)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "piped\n")
	w.Close()
	if e := h.wait(); e.ExitCode != 0 {
		t.Fatalf("exit %+v", e)
	}
	if got := h.logs(); got != "piped\n" {
		t.Fatalf("logs %q", got)
	}
}

func TestTTY(t *testing.T) {
	h := launch(t, []string{"tty"}, func(c *Config) { c.TTY, c.Console = true, [2]uint{24, 80} })
	if e := h.wait(); e.ExitCode != 0 {
		t.Fatalf("exit %+v", e)
	}
	if got := h.logs(); !strings.Contains(got, "tty=true") {
		t.Fatalf("logs %q, want a controlling terminal", got)
	}
}

// A container survives its engine: a new client finds it by its socket.
func TestReconnect(t *testing.T) {
	h := launch(t, []string{"lines"}, func(c *Config) { c.OpenStdin = true })
	c, err := Connect(h.cfg.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pid() != h.client.Pid() || c.Pid() == 0 {
		t.Fatalf("reconnected to pid %d, the container runs as %d", c.Pid(), h.client.Pid())
	}
	w, err := c.Stdin(false)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "after\n")
	w.Close()
	h.client = c
	h.waitLog("got after")
	c.Signal(syscall.SIGTERM)
	if e := h.wait(); e.ExitCode != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit %+v", e)
	}
}

func TestMemoryLimitKillsTheLargestProcess(t *testing.T) {
	h := launch(t, []string{"alloc", "256"}, func(c *Config) { c.Resources.Memory = 32 << 20 })
	e := h.wait()
	if e.ExitCode != 128+int(syscall.SIGKILL) || !e.OOMKilled {
		t.Fatalf("exit %+v, want an OOM kill; logged %q", e, h.logs())
	}
}

func TestPidsLimit(t *testing.T) {
	const limit = 5
	h := launch(t, []string{"forkmany", "12"}, func(c *Config) { c.Resources.PidsLimit = limit })
	h.waitLog("started 12")
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := h.client.State()
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Members) == limit {
			break
		}
		if len(st.Members) < limit || time.Now().After(deadline) {
			t.Fatalf("the container runs %d processes, want the limit, %d", len(st.Members), limit)
		}
		time.Sleep(rescanPeriod)
	}
}

func TestCPUQuota(t *testing.T) {
	h := launch(t, []string{"burn", "3s"}, func(c *Config) { c.Resources.NanoCPUs = 5e8 })
	h.waitLog("cpu ")
	var ms int
	fmt.Sscanf(h.logs()[strings.Index(h.logs(), "cpu "):], "cpu %d", &ms)
	// Half a CPU for three seconds is 1.5s of CPU time; unthrottled the
	// program would use 3s.
	if ms < 900 || ms > 2100 {
		t.Fatalf("the program used %dms of CPU in 3s at half a CPU", ms)
	}
	st, err := h.client.State()
	if err != nil {
		t.Fatal(err)
	}
	if st.Periods == 0 || st.ThrottledPeriods == 0 || st.ThrottledTime == 0 {
		t.Fatalf("throttling not recorded: %+v", st)
	}
}

// The quota changes while the container runs.
func TestUpdate(t *testing.T) {
	h := launch(t, []string{"hold"}, nil)
	if err := h.client.Update(Resources{PidsLimit: 9}, nil); err != nil {
		t.Fatal(err)
	}
	st, err := h.client.State()
	if err != nil || st.Resources.PidsLimit != 9 {
		t.Fatalf("state %+v, %v", st, err)
	}
	nice := 5
	if err := h.client.Update(Resources{PidsLimit: 9}, &nice); err != nil {
		t.Fatal(err)
	}
	if got, err := syscall.Getpriority(syscall.PRIO_PROCESS, h.client.Pid()); err != nil || got != nice {
		t.Fatalf("priority %d, %v; want %d", got, err, nice)
	}
	nice = 0
	if err := h.client.Update(Resources{}, &nice); err == nil {
		t.Fatal("raised the priority back without root")
	}
}

func TestPause(t *testing.T) {
	h := launch(t, []string{"lines"}, func(c *Config) { c.OpenStdin = true })
	if err := h.client.Pause(); err != nil {
		t.Fatal(err)
	}
	w, err := h.client.Stdin(false)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "while paused\n")
	w.Close()
	if err := h.client.Unpause(); err != nil {
		t.Fatal(err)
	}
	h.waitLog("got while paused")
}

func TestPublishedPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := l.Addr().(*net.TCPAddr).Port
	l.Close()
	h := launch(t, []string{"serve", strconv.Itoa(target)}, func(c *Config) {
		c.Spec.Profile.BindPorts = []int{target}
		c.Publish = []Binding{{Proto: "tcp", IP: netip.MustParseAddr("127.0.0.1"), Upstream: net.JoinHostPort("127.0.0.1", strconv.Itoa(target))}}
	})
	h.waitLog("listening")
	if len(h.ready.Ports) != 1 || h.ready.Ports[0] == 0 {
		t.Fatalf("ready %+v, want one bound port", h.ready)
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(h.ready.Ports[0])))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, _ := io.ReadAll(c)
	if string(got) != "hello from the container\n" {
		t.Fatalf("the published port answered %q", got)
	}
}

// A taken host port fails the launch, binding nothing.
func TestPublishedPortInUse(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port
	state, _ := filepath.EvalSymlinks(t.TempDir())
	sockDir, err := os.MkdirTemp("/tmp", "shim")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	cfg := Config{
		Spec:    runtime.Spec{Root: state, Argv: []string{"/work", "hold"}},
		LogPath: filepath.Join(state, "log.jsonl"), Socket: filepath.Join(sockDir, "s.sock"), ExitPath: filepath.Join(state, "exit.json"),
		Publish: []Binding{{Proto: "tcp", IP: netip.MustParseAddr("127.0.0.1"), Lo: port, Hi: port, Upstream: "127.0.0.1:1"}},
	}
	if _, _, err := Launch(state, cfg); !errors.Is(err, ErrPortInUse) {
		t.Fatalf("Launch = %v, want ErrPortInUse", err)
	}
}

func TestDiskIOCounted(t *testing.T) {
	const mib = 16
	h := launch(t, []string{"diskwrite", strconv.Itoa(mib)}, nil)
	h.waitLog("wrote")
	st, err := h.client.State()
	if err != nil {
		t.Fatal(err)
	}
	if st.DiskWritten < mib<<20 {
		t.Fatalf("the container wrote %d bytes to disk by the shim's count, want at least %d", st.DiskWritten, mib<<20)
	}
}

// The kernel's counts of the container's sockets reach the shim's state,
// a connection's after it closed included.
func TestNetworkIOCounted(t *testing.T) {
	const mib = 4
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(c, c)
				c.Close()
			}()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	h := launch(t, []string{"netsend", strconv.Itoa(port), strconv.Itoa(mib)}, func(c *Config) {
		c.Spec.Profile.ConnectPorts = []int{port}
	})
	h.waitLog("sent")
	st, err := h.client.State()
	if err != nil {
		t.Fatal(err)
	}
	if st.Net.TxBytes < mib<<20 || st.Net.RxBytes < mib<<20 || st.Net.TxPackets == 0 || st.Net.RxPackets == 0 {
		t.Fatalf("the container's traffic by the shim's count: %+v, want at least %d bytes each way", st.Net, mib<<20)
	}
}

// The supervisor fwsd runs, fws-shim built as it ships, stays small: a
// container costs one of it.
func TestSupervisorFootprint(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fws-shim")
	build := exec.Command("go", "build", "-o", bin, "./cmd/fws-shim")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fws-shim: %v\n%s", err, out)
	}
	roles.Use(bin)
	defer roles.Use("")
	h := launch(t, []string{"hold"}, nil)
	st, err := h.client.State()
	if err != nil {
		t.Fatal(err)
	}
	u, err := libsystem.ProcUsage(st.Shim)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fws-shim supervising one container: footprint %d KiB, resident %d KiB", u.Footprint>>10, u.Resident>>10)
	if u.Footprint > 16<<20 {
		t.Fatalf("the supervisor's footprint is %d KiB, over 16 MiB", u.Footprint>>10)
	}
}
