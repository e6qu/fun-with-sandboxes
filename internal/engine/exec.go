package engine

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"

	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
)

type Exec struct {
	ID          string
	containerID string
	cfg         container.ExecCreateRequest
	detachKeys  []byte

	mu       sync.Mutex
	started  bool
	running  bool
	exitCode *int
	pid      int
	master   *os.File
	done     chan struct{}
}

func (e *Engine) ExecCreate(ref string, req container.ExecCreateRequest) (string, error) {
	c, err := e.Container(ref)
	if err != nil {
		return "", err
	}
	if !c.isRunning() {
		return "", conflict("container %s is not running", c.rec.ID)
	}
	if c.isPaused() {
		return "", conflict("container %s is paused, unpause the container before exec", c.rec.ID)
	}
	if len(req.Cmd) == 0 {
		return "", invalid("No exec command specified")
	}
	// A privileged container's processes hold what privilege gives here,
	// its engine's socket, so a privileged exec in one gains nothing more,
	// as kind's commands in its nodes run.
	if req.Privileged && !c.rec.HostConfig.Privileged {
		return "", unsupported("privileged exec is supported only in a privileged container, whose processes hold its engine's socket; this engine grants no other privilege")
	}
	if err := e.checkUser(req.User); err != nil {
		return "", err
	}
	keys, ok := parseDetachKeys(req.DetachKeys)
	if !ok {
		return "", fmt.Errorf("Invalid escape keys (%s) provided", req.DetachKeys)
	}
	x := &Exec{ID: newID(), containerID: c.rec.ID, cfg: req, detachKeys: keys, done: make(chan struct{})}
	e.mu.Lock()
	e.execs[x.ID] = x
	e.mu.Unlock()
	e.publishContainer(c, events.ActionExecCreate, map[string]string{"execID": x.ID})
	return x.ID, nil
}

func (e *Engine) exec(id string) (*Exec, *Container, error) {
	e.mu.Lock()
	x, ok := e.execs[id]
	e.mu.Unlock()
	if !ok {
		return nil, nil, notFound("No such exec instance: %s", id)
	}
	c, err := e.Container(x.containerID)
	if err != nil {
		return nil, nil, err
	}
	return x, c, nil
}

// ExecIO connects an exec's standard streams. With a TTY, Stdout receives
// the terminal's output and Stderr is unused.
type ExecIO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// ExecStart runs the exec under the container's profile and blocks until it
// exits.
func (e *Engine) ExecStart(id string, tty bool, size *[2]uint, stdio ExecIO) error {
	x, c, err := e.exec(id)
	if err != nil {
		return err
	}
	x.mu.Lock()
	if x.started {
		x.mu.Unlock()
		return conflict("exec %s has already been started", id)
	}
	x.started = true
	x.mu.Unlock()
	if !c.isRunning() {
		return conflict("container %s is not running", c.rec.ID)
	}
	tty = tty || x.cfg.Tty
	if size == nil {
		size = x.cfg.ConsoleSize
	}

	spec, err := e.profile(c)
	if err != nil {
		return err
	}
	wd := x.cfg.WorkingDir
	if wd == "" {
		wd = c.rec.Config.WorkingDir
	}
	limits, _, err := containerLimits(c.rec.HostConfig.Resources)
	if err != nil {
		return err
	}
	user := x.cfg.User
	if user == "" {
		user = c.rec.Config.User
	}
	env, err := e.processEnv(c, x.cfg.Env, user, tty)
	if err != nil {
		return invalid("%v", err)
	}
	anchor, err := e.anchor(c)
	if err != nil {
		return err
	}
	rs := runtime.Spec{
		Root: c.root(), Argv: x.cfg.Cmd, Env: env, WorkDir: wd,
		Profile: spec, ProfilePath: c.dir + "/exec-" + id[:12] + ".sb", Limits: limits, Library: spec.Libraries[0],
		Anchor: anchor,
	}
	defer os.Remove(rs.ProfilePath)
	var master, slave *os.File
	if tty {
		if master, slave, err = pty.Open(); err != nil {
			return err
		}
		if size != nil && size[0] > 0 && size[1] > 0 {
			pty.Setsize(master, &pty.Winsize{Rows: uint16(size[0]), Cols: uint16(size[1])})
		}
		rs.TTY = slave
	} else {
		if x.cfg.AttachStdin && stdio.Stdin != nil {
			rs.Stdin = stdio.Stdin
		}
		if x.cfg.AttachStdout {
			rs.Stdout = stdio.Stdout
		}
		if x.cfg.AttachStderr {
			rs.Stderr = stdio.Stderr
		}
	}
	proc, err := runtime.Start(rs)
	if slave != nil {
		slave.Close()
	}
	if err != nil {
		if master != nil {
			master.Close()
		}
		code := 126
		x.mu.Lock()
		x.exitCode = &code
		x.mu.Unlock()
		close(x.done)
		return invalid("exec failed: %v", err)
	}
	x.mu.Lock()
	x.running, x.pid, x.master = true, proc.Pid(), master
	x.mu.Unlock()
	// The exec counts against the container's limits from its start.
	c.mu.Lock()
	task := c.task
	c.mu.Unlock()
	if task != nil {
		task.Track()
	}
	e.publishContainer(c, events.ActionExecStart, map[string]string{"execID": id})

	// The exec runs on when its client detaches from its terminal, its
	// output dropped from then on.
	out := &detachable{w: stdio.Stdout}
	detached := make(chan struct{})
	var copied sync.WaitGroup
	if master != nil {
		copied.Add(1)
		go func() {
			defer copied.Done()
			io.Copy(out, master)
		}()
		if x.cfg.AttachStdin && stdio.Stdin != nil {
			go func() {
				if _, err := io.Copy(master, escapable(stdio.Stdin, x.detachKeys)); detachedBy(err) {
					out.detach()
					close(detached)
				}
			}()
		}
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		<-proc.Done()
		proc.KillAll()
		proc.DrainIO(outputDrain)
		if master != nil {
			master.Close()
			copied.Wait()
		}
		code := proc.ExitCode()
		x.mu.Lock()
		x.running, x.exitCode, x.master = false, &code, nil
		x.mu.Unlock()
		close(x.done)
		e.publishContainer(c, events.ActionExecDie, map[string]string{"execID": id, "exitCode": itoa(code)})
	}()
	select {
	case <-finished:
	case <-detached:
		e.publishContainer(c, events.ActionExecDetach, map[string]string{"execID": id})
	}
	return nil
}

func (e *Engine) ExecInspect(id string) (container.ExecInspectResponse, error) {
	x, _, err := e.exec(id)
	if err != nil {
		return container.ExecInspectResponse{}, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	pc := &container.ExecProcessConfig{Tty: x.cfg.Tty, Entrypoint: x.cfg.Cmd[0], Arguments: x.cfg.Cmd[1:], User: x.cfg.User}
	return container.ExecInspectResponse{
		ID: x.ID, Running: x.running, ExitCode: x.exitCode, ProcessConfig: pc,
		OpenStdin: x.cfg.AttachStdin, OpenStdout: x.cfg.AttachStdout, OpenStderr: x.cfg.AttachStderr,
		ContainerID: x.containerID, Pid: x.pid, DetachKeys: x.detachKeys,
	}, nil
}

func (e *Engine) ExecResize(id string, h, w uint16) error {
	x, _, err := e.exec(id)
	if err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.master == nil {
		return conflict("exec %s is not running with a TTY", id)
	}
	return pty.Setsize(x.master, &pty.Winsize{Rows: h, Cols: w})
}

func (e *Engine) Resize(ref string, h, w uint16) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	task := c.task
	c.mu.Unlock()
	if task == nil || !c.rec.Config.Tty {
		return conflict("container %s is not running with a TTY", c.rec.ID)
	}
	return task.Resize(h, w)
}

// probe runs a health check command and returns its exit code and output.
func (e *Engine) probe(c *Container, argv []string, timeout time.Duration) (int, string) {
	spec, err := e.profile(c)
	if err != nil {
		return -1, err.Error()
	}
	limits, _, err := containerLimits(c.rec.HostConfig.Resources)
	if err != nil {
		return -1, err.Error()
	}
	env, err := e.processEnv(c, nil, c.rec.Config.User, false)
	if err != nil {
		return -1, err.Error()
	}
	anchor, err := e.anchor(c)
	if err != nil {
		return -1, err.Error()
	}
	var out limitedBuffer
	rs := runtime.Spec{
		Root: c.root(), Argv: argv, Env: env, WorkDir: c.rec.Config.WorkingDir,
		Profile: spec, ProfilePath: c.dir + "/health-" + newID()[:12] + ".sb", Stdout: &out, Stderr: &out,
		Limits: limits, Library: spec.Libraries[0], Anchor: anchor,
	}
	defer os.Remove(rs.ProfilePath)
	proc, err := runtime.Start(rs)
	if err != nil {
		return -1, err.Error()
	}
	select {
	case <-proc.Done():
		proc.KillAll()
		proc.DrainIO(outputDrain)
		return proc.ExitCode(), out.String()
	case <-time.After(timeout):
		proc.KillAll()
		<-proc.Done()
		proc.DrainIO(outputDrain)
		return -1, "Health check exceeded timeout (" + timeout.String() + ")"
	}
}

// limitedBuffer keeps the first 4096 bytes of output, as Docker keeps for a
// health check result.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := 4096 - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ExecRemove forgets exec session id, as libpod's exec remove does: a
// session still running stays unless force, which kills its process first.
func (e *Engine) ExecRemove(id string, force bool) error {
	x, _, err := e.exec(id)
	if err != nil {
		return err
	}
	x.mu.Lock()
	running, pid := x.running, x.pid
	x.mu.Unlock()
	if running {
		if !force {
			return conflict("exec session %s is still running", id)
		}
		if pid > 0 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		<-x.done
	}
	e.mu.Lock()
	delete(e.execs, id)
	e.mu.Unlock()
	return nil
}
