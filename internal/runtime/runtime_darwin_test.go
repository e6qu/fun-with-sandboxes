package runtime

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
	"github.com/e6qu/fun-with-sandboxes/internal/pathmap"
	"github.com/e6qu/fun-with-sandboxes/internal/procs"
	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A main process that exits while a child still holds its stdout must end the
// container promptly, and KillAll must take the child with it.
func TestExitWithLingeringChild(t *testing.T) {
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(state, "root")
	if out, err := exec.Command("go", "build", "-o", filepath.Join(root, "forker"), "./testdata/forker").CombinedOutput(); err != nil {
		t.Fatalf("build forker: %v\n%s", err, out)
	}
	var out syncBuffer
	p, err := Start(withTestLibrary(t, Spec{
		Root: root, Argv: []string{"/forker"}, Env: []string{"PATH=" + root},
		Profile: seatbelt.Spec{Root: root, StateRoot: state, IPCPrefix: seatbelt.IPCPrefixFor(root)}, ProfilePath: filepath.Join(state, "profile.sb"),
		Stdout: &out, Stderr: &out,
	}))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("process did not finish although its main process exited")
	}
	if p.ExitCode() != 3 {
		t.Fatalf("exit code %d, want 3", p.ExitCode())
	}
	p.KillAll()
	// KillAll returns only once nothing of the group is left to write into
	// the container's files.
	es, err := procs.Own()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.PGID == p.Pid() {
			t.Fatalf("process %d of the group outlived KillAll", e.PID)
		}
	}
	// The child sleeps for an hour holding stdout, so the output drains in
	// time only if KillAll killed it.
	drained := make(chan struct{})
	go func() { p.DrainIO(time.Minute); close(drained) }()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("output did not drain after KillAll")
	}
	if !strings.Contains(out.String(), "parent ") {
		t.Fatalf("output %q lacks the parent's line", out.String())
	}
	if _, err := os.Stat(filepath.Join(state, "profile.sb")); err != nil {
		t.Fatalf("profile not written: %v", err)
	}
}

func limitsSpec(t *testing.T) (Spec, *syncBuffer) {
	t.Helper()
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(state, "root")
	if out, err := exec.Command("go", "build", "-o", filepath.Join(root, "limits"), "./testdata/limits").CombinedOutput(); err != nil {
		t.Fatalf("build limits: %v\n%s", err, out)
	}
	out := &syncBuffer{}
	return withTestLibrary(t, Spec{
		Root: root, Argv: []string{"/limits"}, Env: []string{"PATH=/"},
		Profile:     seatbelt.Spec{Root: root, StateRoot: state, IPCPrefix: seatbelt.IPCPrefixFor(root)},
		ProfilePath: filepath.Join(state, "profile.sb"), Stdout: out, Stderr: out,
	}), out
}

func runToEnd(t *testing.T, s Spec) *Process {
	t.Helper()
	p, err := Start(s)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("process did not finish")
	}
	p.KillAll()
	p.DrainIO(time.Minute)
	return p
}

// The spawn helper applies resource limits, the scheduling priority, the
// disk I/O policy and the fork ceiling before the command runs.
func TestLimitsApply(t *testing.T) {
	s, out := limitsSpec(t)
	s.Limits = Limits{
		// Go programs raise their soft descriptor limit to the hard one as
		// they start, so the probe sees the soft limit only when both match.
		Rlimits:  []Rlimit{{Resource: RlimitNofile, Cur: 64, Max: 64}},
		Nice:     7,
		IOPolicy: libsystem.IOPolicyThrottle,
	}
	if p := runToEnd(t, s); p.ExitCode() != 0 {
		t.Fatalf("exit %d: %s", p.ExitCode(), out.String())
	}
	maxproc, err := unix.SysctlUint32("kern.maxprocperuid")
	if err != nil {
		t.Fatal(err)
	}
	// The fork ceiling lowers the process limit, never raises it.
	var nproc syscall.Rlimit
	if err := syscall.Getrlimit(rlimitNproc, &nproc); err != nil {
		t.Fatal(err)
	}
	ceiling := min(nproc.Cur, ForkCeiling(uint64(maxproc)))
	want := fmt.Sprintf("nofile=64/64 nice=7 iopolicy=%d tty=false nproc=%d", libsystem.IOPolicyThrottle, ceiling)
	if got, _, _ := strings.Cut(strings.TrimSpace(out.String()), " priority="); got != want {
		t.Fatalf("limits %q, want %q", got, want)
	}
}

// probePriority runs the limits probe under the clamp and returns its
// threads' highest base priority.
func probePriority(t *testing.T, clamp QoSClamp) int {
	t.Helper()
	s, out := limitsSpec(t)
	s.Limits = Limits{QoSClamp: clamp}
	if p := runToEnd(t, s); p.ExitCode() != 0 {
		t.Fatalf("exit %d: %s", p.ExitCode(), out.String())
	}
	_, v, ok := strings.Cut(strings.TrimSpace(out.String()), " priority=")
	pri, err := strconv.Atoi(v)
	if !ok || err != nil || pri < 0 {
		t.Fatalf("the probe printed %q", out.String())
	}
	return pri
}

// A QoS clamp holds every thread of the process at most at the class's
// priority, whatever QoS the thread asks for: utility's base priority is
// 20, background's 4, where a default thread's is 31. A CI runner may run
// the tests themselves at utility, so the unclamped probe is held to no
// less than the utility one.
func TestQoSClamp(t *testing.T) {
	free := probePriority(t, QoSNone)
	utility := probePriority(t, QoSUtility)
	background := probePriority(t, QoSBackground)
	if !(background < utility && utility <= free) || utility > 20 {
		t.Fatalf("thread priorities: %d unclamped, %d under utility, %d under background", free, utility, background)
	}
}

var (
	testLibOnce sync.Once
	testLib     string
	testLibErr  error
)

// testLibrary is the container library, with the spawn helper beside it,
// built once for the package's tests.
func testLibrary(t testing.TB) string {
	t.Helper()
	testLibOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fws-runtime-lib")
		if err != nil {
			testLibErr = err
			return
		}
		// Seatbelt matches resolved paths, and the temporary directory is
		// under /var, a link to /private/var.
		if dir, err = filepath.EvalSymlinks(dir); err != nil {
			testLibErr = err
			return
		}
		testLib, testLibErr = pathmap.Build(dir)
	})
	if testLibErr != nil {
		t.Fatal(testLibErr)
	}
	return testLib
}

// A limit the engine's user cannot set fails the start, as a runtime fails
// to create the task.
func TestLimitBeyondTheHardLimitFailsStart(t *testing.T) {
	s, _ := limitsSpec(t)
	var cur syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Max == ^uint64(0) {
		// Lower the hard limit for the test's children, which only root
		// can raise back.
		cur.Max = 1 << 20
	}
	s.Limits.Rlimits = []Rlimit{{Resource: RlimitCore, Cur: 0, Max: cur.Max}, {Resource: RlimitCore, Cur: cur.Max + 1, Max: cur.Max + 1}}
	_, err := Start(s)
	if err == nil || !strings.Contains(err.Error(), "setrlimit core") {
		t.Fatalf("Start = %v, want a setrlimit core failure", err)
	}
}

// With a TTY the process gets the terminal as its controlling terminal.
func TestTTYIsTheControllingTerminal(t *testing.T) {
	s, _ := limitsSpec(t)
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	s.TTY, s.Stdout, s.Stderr = slave, nil, nil
	var out syncBuffer
	copied := make(chan struct{})
	go func() { io.Copy(&out, master); close(copied) }()
	p := runToEnd(t, s)
	slave.Close()
	<-copied
	if p.ExitCode() != 0 || !strings.Contains(out.String(), "tty=true") {
		t.Fatalf("exit %d, output %q: want a controlling terminal", p.ExitCode(), out.String())
	}
}

// An exec the container's anchor starts runs in the main process's
// sandbox, where it may signal the main process; one that enters the same
// profile by itself is a sandbox of its own, and may not.
func TestExecsRunInTheMainProcesssSandbox(t *testing.T) {
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(state, "root")
	if out, err := exec.Command("go", "build", "-o", filepath.Join(root, "signal"), "./testdata/signal").CombinedOutput(); err != nil {
		t.Fatalf("build signal: %v\n%s", err, out)
	}
	// A Unix socket's path is capped at 104 bytes.
	dir, err := os.MkdirTemp("/tmp", "fws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "anchor")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ul := l.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	lf, err := ul.File()
	ul.Close()
	if err != nil {
		t.Fatal(err)
	}
	spec := func(argv ...string) Spec {
		return withTestLibrary(t, Spec{
			Root: root, Argv: argv, Env: []string{"PATH=/"},
			Profile:     seatbelt.Spec{Root: root, StateRoot: state, IPCPrefix: seatbelt.IPCPrefixFor(root)},
			ProfilePath: filepath.Join(state, "profile-"+argv[0][1:]+".sb"),
		})
	}
	ms := spec("/signal")
	ms.AnchorListener = lf
	main, err := Start(ms)
	lf.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { main.KillAll() })
	pid := fmt.Sprint(main.Pid())
	run := func(s Spec) (int, string) {
		var out syncBuffer
		s.Stdout, s.Stderr = &out, &out
		return runToEnd(t, s).ExitCode(), out.String()
	}
	in := spec("/signal", "signal", pid)
	in.Anchor = sock
	if code, out := run(in); code != 0 || !strings.Contains(out, "signalled") {
		t.Errorf("an exec through the anchor: exit %d, %q", code, out)
	}
	if code, out := run(spec("/signal", "signal", pid)); code == 0 {
		t.Errorf("a process sandboxed apart signalled the main process: %q", out)
	}
}

// A start applies the profile compiled ahead of it, compiling nothing
// itself, and a later start of the same profile applies the same compiled
// one; a start whose profile changed compiles it anew.
func TestStartsApplyTheCompiledProfile(t *testing.T) {
	s, out := limitsSpec(t)
	if err := Precompile(s); err != nil {
		t.Fatal(err)
	}
	if p := runToEnd(t, s); p.ExitCode() != 0 {
		t.Fatalf("exit %d: %s", p.ExitCode(), out.String())
	}
	path := compiledPath(s)
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no compiled profile: %v", err)
	}
	if p := runToEnd(t, s); p.ExitCode() != 0 {
		t.Fatalf("second start: exit %d: %s", p.ExitCode(), out.String())
	}
	if again, err := os.Stat(path); err != nil || !os.SameFile(first, again) {
		t.Fatalf("the second start compiled the profile again: %v", err)
	}
	s.Profile.ReadFiles = append(s.Profile.ReadFiles, "/private/etc/hosts")
	if p := runToEnd(t, s); p.ExitCode() != 0 {
		t.Fatalf("start with a changed profile: exit %d: %s", p.ExitCode(), out.String())
	}
	if changed, err := os.Stat(path); err != nil || os.SameFile(first, changed) {
		t.Fatalf("a changed profile kept the old compiled one: %v", err)
	}
	_, text, err := profileText(s)
	if err != nil {
		t.Fatal(err)
	}
	if !applyCompiledFor(t, path, text) {
		t.Fatal("the compiled profile is not of the changed profile's text")
	}
}

// Where macOS refuses a compiled profile's bytecode, as an update of its
// format or of sandbox_apply's would, the spawn helper compiles the
// profile's text itself, and the container starts confined as before.
func TestRefusedBytecodeFallsBackToTheText(t *testing.T) {
	s, out := limitsSpec(t)
	s.Argv = []string{"/limits", "confined"}
	if p := runToEnd(t, s); p.ExitCode() != 0 || strings.TrimSpace(out.String()) != "confined=true" {
		t.Fatalf("exit %d: %s", p.ExitCode(), out.String())
	}
	path := compiledPath(s)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The same hash, so the start takes it as kept, over bytecode of a
	// profile type libsandbox has no such number for.
	bad := append(b[:sha256.Size:sha256.Size], 0xff, 0xff, 0xff, 0x7f, 1, 2, 3, 4)
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if p := runToEnd(t, s); p.ExitCode() != 0 || strings.TrimSpace(out.String()) != "confined=true" {
		t.Fatalf("with refused bytecode: exit %d: %s", p.ExitCode(), out.String())
	}
}

// applyCompiledFor reports whether the compiled profile at path is of text,
// without applying it to the test.
func applyCompiledFor(t *testing.T, path, text string) bool {
	t.Helper()
	return kept(path, profileSum(text))
}

// withTestLibrary gives the spec the container library, which every
// container process loads.
func withTestLibrary(t testing.TB, s Spec) Spec {
	lib := testLibrary(t)
	s.Library = lib
	s.Profile.Libraries = append(s.Profile.Libraries, lib)
	return s
}

// The kernel holds a container's process to its memory limit, and so the
// processes it starts and the execs its anchor starts, with nothing
// watching them: past it they are killed.
func TestMemoryLimitInTheKernel(t *testing.T) {
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(state, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-Werror", "-o", filepath.Join(root, "hog"), "testdata/hog/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build hog: %v\n%s", err, out)
	}
	dir, err := os.MkdirTemp("/tmp", "fws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "anchor")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ul := l.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	lf, err := ul.File()
	ul.Close()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	spec := func(argv ...string) (Spec, *syncBuffer) {
		n++
		out := &syncBuffer{}
		return withTestLibrary(t, Spec{
			Root: root, Argv: argv, Env: []string{"PATH=/"},
			Profile:     seatbelt.Spec{Root: root, StateRoot: state, IPCPrefix: seatbelt.IPCPrefixFor(root)},
			ProfilePath: filepath.Join(state, fmt.Sprintf("profile-%d.sb", n)), Stdout: out, Stderr: out,
			Limits: Limits{MemoryMiB: 64},
		}), out
	}
	// The main process, which starts the anchor, holds.
	ms, _ := spec("/hog", "hold")
	ms.AnchorListener = lf
	main, err := Start(ms)
	lf.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { main.KillAll() })

	// What a process with the limit starts keeps it, through posix_spawn
	// and through fork and exec alike.
	for _, how := range []string{"spawn", "forkexec"} {
		cs, out := spec("/hog", how)
		if p := runToEnd(t, cs); strings.TrimSpace(out.String()) != "child: killed by signal 9" {
			t.Errorf("a child started by %s under a limit: exit %d, %q, want it killed", how, p.ExitCode(), out.String())
		}
	}
	hs, out := spec("/hog")
	if p := runToEnd(t, hs); p.ExitCode() != 128+int(syscall.SIGKILL) {
		t.Errorf("a process at its limit: exit %d, %q", p.ExitCode(), out.String())
	}
	es, out := spec("/hog")
	es.Anchor = sock
	if p := runToEnd(t, es); p.ExitCode() != 128+int(syscall.SIGKILL) {
		t.Errorf("an exec at its limit: exit %d, %q", p.ExitCode(), out.String())
	}
	// A process started without the limit gives the programs it starts
	// the one its container's file holds (pathmap.MemoryFile).
	limits := filepath.Join(state, "limits")
	if err := os.MkdirAll(limits, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(limits, "pids.max"), []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(limits, "memory.max"), []byte("64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, how := range []string{"spawn", "forkexec"} {
		fs, out := spec("/hog", how)
		fs.Limits = Limits{}
		fs.Env = append(fs.Env, "FWS_PIDS="+filepath.Join(limits, "pids.max"))
		fs.Profile.ReadFiles = []string{filepath.Join(limits, "pids.max"), filepath.Join(limits, "memory.max")}
		if p := runToEnd(t, fs); strings.TrimSpace(out.String()) != "child: killed by signal 9" {
			t.Errorf("a child started by %s under the container's memory file: exit %d, %q, want it killed", how, p.ExitCode(), out.String())
		}
	}
	// Without a limit the same process runs on.
	ns, out := spec("/hog")
	ns.Limits = Limits{}
	if p := runToEnd(t, ns); p.ExitCode() != 0 || !strings.Contains(out.String(), "survived") {
		t.Errorf("a process without a limit: exit %d, %q", p.ExitCode(), out.String())
	}
}
