package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
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
	p, err := Start(Spec{
		Root: root, Argv: []string{"/forker"}, Env: []string{"PATH=" + root},
		Profile: seatbelt.Spec{Root: root, StateRoot: state, IPCPrefix: seatbelt.IPCPrefixFor(root), ICMP: true}, ProfilePath: filepath.Join(state, "profile.sb"),
		Stdout: &out, Stderr: &out,
	})
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
	if err := syscall.Kill(-p.Pid(), 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the process group outlived KillAll: kill(-pgid, 0) = %v", err)
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
	return Spec{
		Root: root, Argv: []string{"/limits"}, Env: []string{"PATH=/"},
		Profile:     seatbelt.Spec{Root: root, StateRoot: state, IPCPrefix: seatbelt.IPCPrefixFor(root)},
		ProfilePath: filepath.Join(state, "profile.sb"), Stdout: out, Stderr: out,
	}, out
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

// The spawn helper applies resource limits, the scheduling priority and the
// disk I/O policy before the command runs.
func TestLimitsApply(t *testing.T) {
	s, out := limitsSpec(t)
	s.Limits = Limits{
		Rlimits:  []Rlimit{{Resource: RlimitNofile, Cur: 64, Max: 128}},
		Nice:     7,
		IOPolicy: libsystem.IOPolicyThrottle,
	}
	if p := runToEnd(t, s); p.ExitCode() != 0 {
		t.Fatalf("exit %d: %s", p.ExitCode(), out.String())
	}
	want := fmt.Sprintf("nofile=64/128 nice=7 iopolicy=%d tty=false", libsystem.IOPolicyThrottle)
	if got := strings.TrimSpace(out.String()); got != want {
		t.Fatalf("limits %q, want %q", got, want)
	}
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
