package libsystem

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperEnv makes the test binary act as a probe child instead of running
// tests, so the probes measure a real spawned process.
const helperEnv = "FWS_LIBSYSTEM_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "sleep":
		time.Sleep(time.Minute)
	case "iopolicy":
		p, err := IOPolicy()
		fmt.Println(p, err)
	case "limits":
		json.NewEncoder(os.Stdout).Encode(probeLimits())
	case "shm":
		for _, name := range strings.Split(os.Getenv("FWS_SHM_NAMES"), ",") {
			fd, err := ShmOpen(name, os.O_CREATE|os.O_RDWR, 0o600)
			if err == nil {
				syscall.Close(fd)
				ShmUnlink(name)
			}
			fmt.Printf("%s %v\n", name, err)
		}
	}
	os.Exit(0)
}

// spawnHelper starts the test binary in helper mode with its output on a
// pipe.
func spawnHelper(t *testing.T, mode string, disclaim bool, extraEnv ...string) (int, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	env := append(append(os.Environ(), helperEnv+"="+mode), extraEnv...)
	pid, err := Spawn(os.Args[0], []string{os.Args[0]}, env, SpawnAttr{
		Dir: t.TempDir(), Files: []uintptr{devnull.Fd(), w.Fd(), w.Fd()}, Disclaim: disclaim,
	})
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return pid, r
}

func wait(t *testing.T, pid int) syscall.WaitStatus {
	t.Helper()
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		t.Fatal(err)
	}
	return ws
}

// A spawned process with responsibility disclaimed answers for itself in
// privacy decisions; one without answers through its parent's chain.
func TestSpawnDisclaimsResponsibility(t *testing.T) {
	mine := ResponsiblePID(os.Getpid())
	for _, disclaim := range []bool{true, false} {
		pid, out := spawnHelper(t, "sleep", disclaim)
		got := ResponsiblePID(pid)
		syscall.Kill(pid, syscall.SIGKILL)
		wait(t, pid)
		out.Close()
		t.Logf("disclaim=%v: child %d responsible %d; this process's responsible %d", disclaim, pid, got, mine)
		if disclaim && got != pid {
			t.Errorf("a disclaiming child %d is answered for by %d", pid, got)
		}
		if !disclaim && got != mine {
			t.Errorf("a child that did not disclaim is answered for by %d, not by %d", got, mine)
		}
	}
}

// Spawn starts the process in the directory asked, with the descriptors
// asked and no other.
func TestSpawnDirAndDescriptors(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, w, _ := os.Pipe()
	leak, _ := os.Open(os.DevNull)
	defer leak.Close()
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	pid, err := Spawn("/bin/sh", []string{"sh", "-c", "pwd; ls /dev/fd | wc -l"}, []string{"PATH=/bin:/usr/bin"},
		SpawnAttr{Dir: dir, Files: []uintptr{devnull.Fd(), w.Fd(), w.Fd()}})
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(r)
	var lines []string
	for sc.Scan() {
		lines = append(lines, strings.TrimSpace(sc.Text()))
	}
	if ws := wait(t, pid); ws.ExitStatus() != 0 {
		t.Fatalf("sh exited %d", ws.ExitStatus())
	}
	// ls holds its own descriptor on /dev/fd as it lists it.
	if len(lines) != 2 || lines[0] != dir || lines[1] != "4" {
		t.Fatalf("the child saw %q; want its directory %s and descriptors 0-2 plus ls's own", lines, dir)
	}
}

func TestProcUsage(t *testing.T) {
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
	}
	u, err := ProcUsage(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("usage of this process: %+v", u)
	if u.CPU < uint64(40*time.Millisecond) || u.Footprint == 0 || u.Resident == 0 {
		t.Fatalf("usage %+v: want at least 40ms of CPU and a footprint", u)
	}
	if _, err := ProcUsage(1 << 30); err == nil {
		t.Fatal("usage of a process that does not exist")
	}
}

// The disk I/O policy set on a process passes to the processes it spawns.
func TestIOPolicyIsInherited(t *testing.T) {
	before, err := IOPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if err := SetIOPolicy(IOPolicyThrottle); err != nil {
		t.Fatal(err)
	}
	defer SetIOPolicy(before)
	pid, out := spawnHelper(t, "iopolicy", false)
	b := make([]byte, 64)
	n, _ := out.Read(b)
	out.Close()
	wait(t, pid)
	if got := strings.TrimSpace(string(b[:n])); got != strconv.Itoa(IOPolicyThrottle)+" <nil>" {
		t.Fatalf("the child reports I/O policy %q, want %d", got, IOPolicyThrottle)
	}
}

type limitResult struct {
	Resource string
	Enforced bool
	Detail   string
}

// probeLimits sets each resource limit low in this process and reports
// whether the kernel holds it to it.
func probeLimits() []limitResult {
	var out []limitResult
	set := func(res int, v uint64) error {
		return syscall.Setrlimit(res, &syscall.Rlimit{Cur: v, Max: v})
	}
	// RLIMIT_NOFILE: open past it.
	if err := set(syscall.RLIMIT_NOFILE, 16); err != nil {
		out = append(out, limitResult{"nofile", false, err.Error()})
	} else {
		var err error
		for i := 0; i < 32 && err == nil; i++ {
			_, err = os.Open(os.DevNull)
		}
		out = append(out, limitResult{"nofile", errors.Is(err, syscall.EMFILE), fmt.Sprint(err)})
	}
	// RLIMIT_FSIZE: write past it, with SIGXFSZ ignored so write reports it.
	signalIgnore(syscall.SIGXFSZ)
	if err := set(syscall.RLIMIT_FSIZE, 4096); err != nil {
		out = append(out, limitResult{"fsize", false, err.Error()})
	} else {
		f, ferr := os.CreateTemp("", "fsize")
		var werr error
		if ferr == nil {
			_, werr = f.Write(make([]byte, 8192))
			f.Close()
			os.Remove(f.Name())
		}
		out = append(out, limitResult{"fsize", errors.Is(werr, syscall.EFBIG), fmt.Sprint(ferr, werr)})
	}
	// RLIMIT_DATA and RLIMIT_AS: allocate past them.
	for _, r := range []struct {
		name string
		res  int
	}{{"data", syscall.RLIMIT_DATA}, {"as", 5}} {
		var cur syscall.Rlimit
		syscall.Getrlimit(r.res, &cur)
		err := set(r.res, 256<<20)
		if err != nil {
			out = append(out, limitResult{r.name, false, err.Error()})
			continue
		}
		_, merr := syscall.Mmap(-1, 0, 512<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
		out = append(out, limitResult{r.name, merr != nil, fmt.Sprint(merr)})
		syscall.Setrlimit(r.res, &cur)
	}
	return out
}

// The kernel enforces RLIMIT_NOFILE and RLIMIT_FSIZE; the probe also
// reports the memory limits macOS is known to leave unenforced.
func TestResourceLimitsEnforced(t *testing.T) {
	pid, out := spawnHelper(t, "limits", false)
	var results []limitResult
	err := json.NewDecoder(out).Decode(&results)
	out.Close()
	wait(t, pid)
	if err != nil {
		t.Fatal(err)
	}
	enforced := map[string]bool{}
	for _, r := range results {
		t.Logf("rlimit %s: enforced=%v (%s)", r.Resource, r.Enforced, r.Detail)
		enforced[r.Resource] = r.Enforced
	}
	for _, want := range []string{"nofile", "fsize"} {
		if !enforced[want] {
			t.Errorf("RLIMIT_%s is not enforced", strings.ToUpper(want))
		}
	}
}

// ShmProbe reports, for each name, whether the process could create it.
func shmProbe(t *testing.T, names []string, wrap func(argv []string) []string) map[string]string {
	t.Helper()
	argv := wrap([]string{os.Args[0]})
	r, w, _ := os.Pipe()
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	env := append(os.Environ(), helperEnv+"=shm", "FWS_SHM_NAMES="+strings.Join(names, ","))
	pid, err := Spawn(argv[0], argv, env, SpawnAttr{Files: []uintptr{devnull.Fd(), w.Fd(), w.Fd()}})
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, res, _ := strings.Cut(sc.Text(), " ")
		got[name] = res
	}
	wait(t, pid)
	return got
}

// Seatbelt filters POSIX shared memory names by prefix, which isolates them
// per container as an IPC namespace does.
func TestSeatbeltFiltersSharedMemoryNames(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "shm.sb")
	os.WriteFile(profile, []byte(`(version 1)
(allow default)
(deny ipc-posix-shm*)
(allow ipc-posix-shm* (ipc-posix-name-prefix "fws1."))
`), 0o600)
	tag := strconv.Itoa(os.Getpid())
	inside, outside := "fws1."+tag, "other."+tag
	got := shmProbe(t, []string{inside, outside}, func(argv []string) []string {
		return append([]string{"/usr/bin/sandbox-exec", "-f", profile}, argv...)
	})
	t.Logf("shared memory under the prefix profile: %v", got)
	if got[inside] != "<nil>" {
		t.Errorf("a name under the prefix was refused: %s", got[inside])
	}
	if got[outside] == "<nil>" {
		t.Errorf("a name outside the prefix was allowed")
	}
}
