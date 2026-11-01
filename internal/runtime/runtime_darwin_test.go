package runtime

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
		Profile: seatbelt.Spec{Root: root, StateRoot: state}, ProfilePath: filepath.Join(state, "profile.sb"),
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
