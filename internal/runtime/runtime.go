// Package runtime launches a container's processes on the host under the
// container's Seatbelt profile.
package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/procs"
	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
)

type Spec struct {
	// Root is the container's root directory; Argv[0] and WorkDir are paths
	// inside it.
	Root    string
	Argv    []string
	Env     []string
	WorkDir string
	// Profile is the profile the process runs under; ProfilePath is where
	// its text is kept, outside the container root, for inspection.
	Profile     seatbelt.Spec
	ProfilePath string
	Stdin       io.Reader `json:"-"`
	Stdout      io.Writer `json:"-"`
	Stderr      io.Writer `json:"-"`
	// TTY, when set, is the pty slave that becomes the process's controlling
	// terminal and all three standard streams.
	TTY *os.File `json:"-"`
	// Limits apply to the process and everything it starts.
	Limits Limits
	// Library is the container library (package pathmap) every process
	// of the container loads; the profile must let it read the library.
	Library string
}

// Limits are what the spawn helper applies to the process before it
// executes the container's command, and what its descendants inherit.
type Limits struct {
	Rlimits []Rlimit `json:",omitempty"`
	// Nice is the scheduling priority, 0 to 19.
	Nice int `json:",omitempty"`
	// IOPolicy is a disk I/O policy (libsystem.IOPolicy*), or 0 to keep
	// the default.
	IOPolicy int `json:",omitempty"`
}

// Rlimit is a resource limit, as setrlimit takes it.
type Rlimit struct {
	Resource int
	Cur, Max uint64
}

type Process struct {
	pid  int
	proc *os.Process
	done chan struct{}
	io   sync.WaitGroup
	// readEnds are the engine's ends of the output pipes.
	readEnds []*os.File
	mu       sync.Mutex
	code     int
}

// Resolve maps argv[0] to the executable's host path inside root, searching the
// container's PATH when argv[0] has no slash.
func Resolve(root, argv0 string, env []string) (string, error) {
	if argv0 == "" {
		return "", errors.New("no command specified")
	}
	if strings.Contains(argv0, "/") {
		p := filepath.Join(root, path.Clean("/"+argv0))
		if !isExecutable(p) {
			return "", fmt.Errorf("exec: %q: no such executable in the container", argv0)
		}
		return p, nil
	}
	pathVar := "/usr/local/bin:/usr/bin:/bin"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVar = v
		}
	}
	for _, dir := range filepath.SplitList(pathVar) {
		if !strings.HasPrefix(dir, "/") {
			continue
		}
		base := filepath.Join(root, path.Clean(dir))
		if strings.HasPrefix(dir, root+"/") {
			base = path.Clean(dir)
		}
		p := filepath.Join(base, argv0)
		if isExecutable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", argv0)
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// DrainIO waits for the output pipes to drain, which happens once every
// process holding them has exited. A descendant that left the process group
// can hold them open forever, so after max the engine stops reading, as
// os/exec's WaitDelay does.
func (p *Process) DrainIO(max time.Duration) {
	drained := make(chan struct{})
	go func() {
		p.io.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(max):
		closeAll(p.readEnds)
		<-drained
	}
}

func closeAll(fs []*os.File) {
	for _, f := range fs {
		f.Close()
	}
}

func (p *Process) wait() {
	code := 255
	if st, err := p.proc.Wait(); err == nil {
		ws := st.Sys().(syscall.WaitStatus)
		switch {
		case ws.Signaled():
			code = 128 + int(ws.Signal())
		default:
			code = ws.ExitStatus()
		}
	}
	p.mu.Lock()
	p.code = code
	p.mu.Unlock()
	close(p.done)
}

func (p *Process) Pid() int { return p.pid }

func (p *Process) Done() <-chan struct{} { return p.done }

// ExitCode is valid once Done is closed.
func (p *Process) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code
}

// Signal delivers sig to the main process only, as Docker does for stop.
func (p *Process) Signal(sig syscall.Signal) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	return p.proc.Signal(sig)
}

// KillAll sends SIGKILL to the whole process group and returns once the
// group is gone, so no process of it still writes to the container's files.
func (p *Process) KillAll() {
	pgid := p.pid
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		p.proc.Kill()
		return
	}
	// macOS has no notification for a process group emptying out, but it
	// reports each process's exit: wait on the group's members.
	es, err := procs.Own()
	if err != nil {
		return
	}
	var members []int
	for _, e := range es {
		if e.PGID == pgid {
			members = append(members, e.PID)
		}
	}
	procs.WaitExit(members, groupExitTimeout)
}

const groupExitTimeout = 10 * time.Second
