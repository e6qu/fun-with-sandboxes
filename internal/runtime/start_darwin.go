package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
	"github.com/e6qu/fun-with-sandboxes/internal/roles"
	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
)

// The engine starts every container process through a helper: itself,
// spawned with these variables, which applies the process's limits and its
// Seatbelt profile, and executes the command with the container library
// loaded. posix_spawn cannot set resource limits or a controlling
// terminal, Go cannot run code between fork and exec, and sandbox-exec, an
// Apple binary, has the loader drop the variables that load the library.
const (
	RoleVar    = roles.Var
	spawnRole  = "spawn"
	helperVar  = "FWS_SPAWN"
	statusFD   = 3
	helperArgv = "fws-spawn"
)

type helperSpec struct {
	Path    string
	Argv    []string
	Profile string
	Library string
	Limits  Limits
	TTY     bool
}

func init() {
	if os.Getenv(RoleVar) == spawnRole {
		runHelper()
	}
}

// runHelper applies the limits to itself and executes the command. It
// reports a failure on descriptor 3, which closes on a successful exec.
func runHelper() {
	status := os.NewFile(statusFD, "status")
	fail := func(err error) {
		status.WriteString(err.Error())
		os.Exit(127)
	}
	var h helperSpec
	if err := json.Unmarshal([]byte(os.Getenv(helperVar)), &h); err != nil {
		fail(fmt.Errorf("spawn helper: %w", err))
	}
	if h.Path == "" || len(h.Argv) == 0 {
		fail(errors.New("spawn helper: no command"))
	}
	if err := applyLimits(h); err != nil {
		fail(err)
	}
	env := withoutVars(os.Environ(), RoleVar, helperVar, "DYLD_INSERT_LIBRARIES")
	if h.Library != "" {
		env = append(env, "DYLD_INSERT_LIBRARIES="+h.Library)
	}
	if err := libsystem.SandboxInit(h.Profile); err != nil {
		fail(err)
	}
	syscall.CloseOnExec(statusFD)
	err := syscall.Exec(h.Path, h.Argv, env)
	fail(fmt.Errorf("exec %s: %w", h.Argv[0], err))
}

func applyLimits(h helperSpec) error {
	if h.TTY {
		if err := unix.IoctlSetInt(0, unix.TIOCSCTTY, 0); err != nil {
			return fmt.Errorf("set the controlling terminal: %w", err)
		}
	}
	if h.Limits.Nice != 0 {
		if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, h.Limits.Nice); err != nil {
			return fmt.Errorf("setpriority %d: %w", h.Limits.Nice, err)
		}
	}
	if h.Limits.IOPolicy != 0 {
		if err := libsystem.SetIOPolicy(h.Limits.IOPolicy); err != nil {
			return fmt.Errorf("setiopolicy_np %d: %w", h.Limits.IOPolicy, err)
		}
	}
	for _, r := range h.Limits.Rlimits {
		if err := syscall.Setrlimit(r.Resource, &syscall.Rlimit{Cur: r.Cur, Max: r.Max}); err != nil {
			return fmt.Errorf("setrlimit %s soft %d hard %d: %w", RlimitName(r.Resource), r.Cur, r.Max, err)
		}
	}
	return nil
}

func withoutVars(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
next:
	for _, kv := range env {
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

// Start launches the process. The process leads its own process group, so
// KillAll reaches everything it forks that stays in the group, and it is
// responsible for itself, so it inherits no privacy grant of whatever
// launched the engine.
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
	self, err := roles.Binary()
	if err != nil {
		return nil, err
	}
	exe, argv := scriptCommand(s.Root, exe, s.Argv, s.Env)
	hs, err := json.Marshal(helperSpec{
		Path: exe, Argv: argv, Profile: text, Library: s.Library,
		Limits: s.Limits, TTY: s.TTY != nil,
	})
	if err != nil {
		return nil, err
	}
	for _, kv := range s.Env {
		if strings.HasPrefix(kv, RoleVar+"=") || strings.HasPrefix(kv, helperVar+"=") {
			return nil, fmt.Errorf("the engine reserves the variable %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	env := append(append([]string{}, s.Env...), RoleVar+"="+spawnRole, helperVar+"="+string(hs))

	p := &Process{done: make(chan struct{})}
	// The process gets real pipes: with plain writers the engine would wait
	// for every descendant to close its copy of stdout, and for the stdin
	// source to reach EOF.
	var childEnds, parentEnds []*os.File
	cleanup := func() {
		closeAll(childEnds)
		closeAll(parentEnds)
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	childEnds = append(childEnds, null)
	stdio := [3]*os.File{null, null, null}
	if s.TTY != nil {
		stdio = [3]*os.File{s.TTY, s.TTY, s.TTY}
	} else {
		for i, dst := range []io.Writer{s.Stdout, s.Stderr} {
			if dst == nil {
				continue
			}
			r, w, err := os.Pipe()
			if err != nil {
				cleanup()
				return nil, err
			}
			stdio[i+1] = w
			childEnds, parentEnds = append(childEnds, w), append(parentEnds, r)
		}
		if s.Stdin != nil {
			r, w, err := os.Pipe()
			if err != nil {
				cleanup()
				return nil, err
			}
			stdio[0] = r
			childEnds, parentEnds = append(childEnds, r), append(parentEnds, w)
		}
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	defer statusR.Close()
	pid, err := libsystem.Spawn(self, []string{helperArgv}, env, libsystem.SpawnAttr{
		Dir:   wd,
		Files: []uintptr{stdio[0].Fd(), stdio[1].Fd(), stdio[2].Fd(), statusW.Fd()},
		// A session leader also leads its process group, so KillAll still
		// reaches every descendant.
		SetSID:   s.TTY != nil,
		Disclaim: true,
	})
	statusW.Close()
	closeAll(childEnds)
	if err != nil {
		closeAll(parentEnds)
		return nil, err
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		closeAll(parentEnds)
		return nil, err
	}
	failure, _ := io.ReadAll(statusR)
	if len(failure) > 0 {
		proc.Wait()
		closeAll(parentEnds)
		return nil, errors.New(string(failure))
	}
	p.pid, p.proc = pid, proc
	if s.TTY == nil {
		k := 0
		for _, dst := range []io.Writer{s.Stdout, s.Stderr} {
			if dst == nil {
				continue
			}
			r := parentEnds[k]
			k++
			p.readEnds = append(p.readEnds, r)
			p.io.Add(1)
			go func(dst io.Writer, r *os.File) {
				defer p.io.Done()
				defer r.Close()
				io.Copy(dst, r)
			}(dst, r)
		}
		if s.Stdin != nil {
			w := parentEnds[k]
			go func() {
				defer w.Close()
				io.Copy(w, s.Stdin)
			}()
		}
	}
	go p.wait()
	return p, nil
}
