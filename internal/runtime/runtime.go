// Package runtime launches a container's processes on the host under the
// container's Seatbelt profile.
package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
)

// SandboxExec is the macOS tool that applies a profile and execs the command.
const SandboxExec = "/usr/bin/sandbox-exec"

type Spec struct {
	// Root is the container's root directory; Argv[0] and WorkDir are paths
	// inside it.
	Root    string
	Argv    []string
	Env     []string
	WorkDir string
	// Profile is the rendered profile; ProfilePath is where it is written for
	// sandbox-exec, outside the container root.
	Profile     seatbelt.Spec
	ProfilePath string
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	// TTY, when set, is the pty slave that becomes the process's controlling
	// terminal and all three standard streams.
	TTY *os.File
}

type Process struct {
	cmd  *exec.Cmd
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

// Start launches the process. The process leads its own process group, so
// KillAll reaches everything it forks that stays in the group.
func Start(s Spec) (*Process, error) {
	if len(s.Argv) == 0 {
		return nil, errors.New("no command specified")
	}
	exe, err := Resolve(s.Root, s.Argv[0], s.Env)
	if err != nil {
		return nil, err
	}
	text, err := seatbelt.Render(s.Profile)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.ProfilePath, []byte(text), 0o600); err != nil {
		return nil, err
	}
	wd := filepath.Join(s.Root, path.Clean("/"+s.WorkDir))
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return nil, err
	}
	cmd := exec.Command(SandboxExec, append([]string{"-f", s.ProfilePath, exe}, s.Argv[1:]...)...)
	cmd.Env = s.Env
	cmd.Dir = wd
	p := &Process{cmd: cmd, done: make(chan struct{})}
	// The process gets real pipes: with plain writers os/exec would hold Wait
	// until every descendant closed its copy of stdout, and until the stdin
	// source hit EOF.
	var childEnds, parentEnds []*os.File
	fail := func(err error) (*Process, error) {
		closeAll(childEnds)
		closeAll(parentEnds)
		return nil, err
	}
	if s.TTY != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = s.TTY, s.TTY, s.TTY
		// A session leader also leads its process group, so KillAll still
		// reaches every descendant.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		for _, o := range []struct {
			dst io.Writer
			set *io.Writer
		}{{s.Stdout, &cmd.Stdout}, {s.Stderr, &cmd.Stderr}} {
			if o.dst == nil {
				continue
			}
			r, w, err := os.Pipe()
			if err != nil {
				return fail(err)
			}
			*o.set = w
			childEnds, parentEnds = append(childEnds, w), append(parentEnds, r)
			p.io.Add(1)
			go func(dst io.Writer, r *os.File) {
				defer p.io.Done()
				defer r.Close()
				io.Copy(dst, r)
			}(o.dst, r)
		}
		if s.Stdin != nil {
			r, w, err := os.Pipe()
			if err != nil {
				return fail(err)
			}
			cmd.Stdin = r
			childEnds = append(childEnds, r)
			go func() {
				defer w.Close()
				io.Copy(w, s.Stdin)
			}()
		}
	}
	if err := cmd.Start(); err != nil {
		closeAll(childEnds)
		// The copy goroutines own the read ends and finish once the write
		// ends close.
		p.io.Wait()
		return nil, err
	}
	closeAll(childEnds)
	p.readEnds = parentEnds
	go p.wait()
	return p, nil
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
	err := p.cmd.Wait()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		ws := ee.Sys().(syscall.WaitStatus)
		switch {
		case ws.Signaled():
			code = 128 + int(ws.Signal())
		default:
			code = ws.ExitStatus()
		}
	} else if err != nil {
		code = 255
	}
	p.mu.Lock()
	p.code = code
	p.mu.Unlock()
	close(p.done)
}

func (p *Process) Pid() int { return p.cmd.Process.Pid }

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
	return p.cmd.Process.Signal(sig)
}

// KillAll sends SIGKILL to the whole process group and returns once the
// group is gone, so no process of it still writes to the container's files.
func (p *Process) KillAll() {
	pgid := p.cmd.Process.Pid
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		p.cmd.Process.Kill()
		return
	}
	// macOS has no notification for a process group emptying out, and the
	// group's other members are not the engine's children to wait for, so
	// probe it the way kill(1) does. Killed processes leave within
	// milliseconds; the main process stays until Wait reaps it.
	deadline := time.Now().Add(groupExitTimeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(groupExitProbe)
	}
}

const (
	groupExitTimeout = 10 * time.Second
	groupExitProbe   = 2 * time.Millisecond
)
