package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/execpath"
	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
	"github.com/e6qu/fun-with-sandboxes/internal/pathmap"
	"github.com/e6qu/fun-with-sandboxes/internal/roles"
)

// The engine starts every container process through the spawn helper, a C
// program built beside the container library (pathmap/src/spawn.c): it
// applies the process's limits and its compiled Seatbelt profile to
// itself, starts the container's exec anchor where asked, and executes the
// command with the container library loaded. posix_spawn cannot set
// resource limits or a controlling terminal, and sandbox-exec, an Apple
// binary, has the loader drop the variables that load the library.
const (
	RoleVar  = roles.Var
	statusFD = 3
)

// spawnArgs are the spawn helper's arguments for starting the command at
// path with argv: its options, then the command.
func spawnArgs(s Spec, path string, argv []string, compiled string, ceiling uint64) []string {
	args := []string{"fws-spawn"}
	if s.TTY != nil {
		args = append(args, "-t")
	}
	if s.Limits.Nice != 0 {
		args = append(args, "-n", strconv.Itoa(s.Limits.Nice))
	}
	if s.Limits.IOPolicy != 0 {
		args = append(args, "-i", strconv.Itoa(s.Limits.IOPolicy))
	}
	for _, r := range s.Limits.Rlimits {
		args = append(args, "-r", fmt.Sprintf("%d:%d:%d", r.Resource, r.Cur, r.Max))
	}
	args = append(args, "-N", strconv.FormatUint(ceiling, 10), "-c", compiled, "-L", s.Library)
	if s.AnchorListener != nil {
		args = append(args, "-a", s.Profile.Helper)
	}
	return append(append(args, "--", path), argv...)
}

// forkCeiling is the process limit a container's processes get: below the
// user's own, kern.maxprocperuid, by ForkCeiling's reserve, so a fork bomb
// fails in the kernel, at no cost to any fork short of it, while the
// user's own programs can still start.
func forkCeiling() (uint64, error) {
	max, err := unix.SysctlUint32("kern.maxprocperuid")
	if err != nil {
		return 0, fmt.Errorf("read the user's process limit: %w", err)
	}
	return ForkCeiling(uint64(max)), nil
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
	if s.Library == "" {
		return nil, errors.New("runtime: a container's processes run with the container library, and the spec names none")
	}
	exe, err := execpath.Resolve(s.Root, execpath.FromWorkDir(s.WorkDir, s.Argv[0]), s.Env)
	if err != nil {
		return nil, err
	}
	profile, text, err := profileText(s)
	if err != nil {
		return nil, err
	}
	s.Profile = profile
	if err := os.WriteFile(s.ProfilePath, []byte(text), 0o600); err != nil {
		return nil, err
	}
	wd := filepath.Join(s.Root, path.Clean("/"+s.WorkDir))
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return nil, err
	}
	exe, argv := scriptCommand(s.Root, exe, s.Argv, s.Env)
	for _, kv := range s.Env {
		if strings.HasPrefix(kv, RoleVar+"=") {
			return nil, fmt.Errorf("the engine reserves the variable %s", RoleVar)
		}
	}

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
	if s.Anchor != "" {
		req := anchorRequest{Path: exe, Argv: argv, Env: withLibrary(s.Env, s.Library), Dir: wd, TTY: s.TTY != nil}
		pid, dec, conn, err := startInAnchor(s.Anchor, req, stdio)
		closeAll(childEnds)
		if err != nil {
			closeAll(parentEnds)
			return nil, err
		}
		p.pid = pid
		p.copyIO(s, parentEnds)
		go func() {
			defer conn.Close()
			code := 255
			var r anchorReply
			if dec.Decode(&r) == nil && r.Code != nil {
				code = *r.Code
			}
			p.finish(code)
		}()
		return p, nil
	}
	// A process the helper starts applies the profile compiled for its
	// text; an exec runs in the container's sandbox already, through its
	// anchor, above.
	compiledPath, err := compiled(s, text)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("compile the container's profile: %w", err)
	}
	ceiling, err := forkCeiling()
	if err != nil {
		cleanup()
		return nil, err
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	defer statusR.Close()
	files := []uintptr{stdio[0].Fd(), stdio[1].Fd(), stdio[2].Fd(), statusW.Fd()}
	if s.AnchorListener != nil {
		files = append(files, s.AnchorListener.Fd())
	}
	// The helper loads no container library itself, which would map its
	// own paths into the container; the command gets it.
	pid, err := libsystem.Spawn(pathmap.Spawner(s.Library), spawnArgs(s, exe, argv, compiledPath, ceiling), containerEnv(s.Env), libsystem.SpawnAttr{
		Dir:   wd,
		Files: files,
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
	p.copyIO(s, parentEnds)
	go p.wait()
	return p, nil
}

// copyIO copies the process's output pipes to their writers, and its stdin
// source to its stdin pipe.
func (p *Process) copyIO(s Spec, parentEnds []*os.File) {
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
}

// containerEnv is env without the variables the engine sets for its own
// processes.
func containerEnv(env []string) []string {
	return withoutVars(env, RoleVar, initVar, "DYLD_INSERT_LIBRARIES")
}

// withLibrary is env with the container library loaded, as the spawn helper
// gives it a command, for an exec the anchor starts.
func withLibrary(env []string, library string) []string {
	return append(containerEnv(env), "DYLD_INSERT_LIBRARIES="+library)
}
