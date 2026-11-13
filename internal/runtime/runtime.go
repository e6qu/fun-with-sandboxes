// Package runtime launches a container's processes on the host under the
// container's Seatbelt profile.
package runtime

import (
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/execpath"
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
	// AnchorListener, for a container's main process, is the listening
	// socket of the anchor its sandbox starts, which runs its execs.
	AnchorListener *os.File `json:"-"`
	// Anchor, for an exec, is the socket of its container's anchor, which
	// starts it inside the container's sandbox.
	Anchor string `json:"-"`
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

// exitCode is a wait status as a shell reports it.
func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

// InitPath is where a container run with --init finds its init, as
// Docker's docker-init: the engine's helper binary, which takes the init's
// role when it runs by that name.
const InitPath = "/sbin/docker-init"

// FromWorkDir makes a relative argv[0] with a slash absolute in the
// container (package execpath).
func FromWorkDir(workDir, argv0 string) string { return execpath.FromWorkDir(workDir, argv0) }

// Resolve maps argv[0] to the executable's host path inside root (package
// execpath).
func Resolve(root, argv0 string, env []string) (string, error) {
	return execpath.Resolve(root, argv0, env)
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
		code = exitCode(st.Sys().(syscall.WaitStatus))
	}
	p.finish(code)
}

func (p *Process) finish(code int) {
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
	return syscall.Kill(p.pid, sig)
}

// KillAll sends SIGKILL to the whole process group and returns once the
// group is gone, so no process of it still writes to the container's files.
func (p *Process) KillAll() {
	pgid := p.pid
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		syscall.Kill(p.pid, syscall.SIGKILL)
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

// The roles of a container's exec anchor and of the launcher that starts
// it, which run in the container's sandbox.
const (
	anchorLaunchRole = "anchor-launch"
	anchorRole       = "anchor"
)

// IsAnchor reports whether env is the environment of a container's exec
// anchor or its launcher: processes of the engine's in the container's
// sandbox, which are not the container's own.
func IsAnchor(env []string) bool {
	for _, kv := range env {
		if kv == RoleVar+"="+anchorRole || kv == RoleVar+"="+anchorLaunchRole {
			return true
		}
	}
	return false
}
