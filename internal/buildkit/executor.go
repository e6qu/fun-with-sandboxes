package buildkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/executor"
	resourcestypes "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/executor/resources/types"
	gatewayapi "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend/gateway/pb"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/snapshot"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/pb"

	"github.com/e6qu/fun-with-sandboxes/internal/egress"
	"github.com/e6qu/fun-with-sandboxes/internal/identity"
	"github.com/e6qu/fun-with-sandboxes/internal/pathmap"
	"github.com/e6qu/fun-with-sandboxes/internal/procs"
	"github.com/e6qu/fun-with-sandboxes/internal/rootfs"
	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
)

// outputDrain bounds reading output after a process group is gone, for
// descendants that escaped the group.
const outputDrain = 2 * time.Second

// execOpt is what the executor needs from the engine.
type execOpt struct {
	// StateRoot is the engine's state directory; profiles deny it.
	StateRoot string
	// ProfileDir holds the profiles the executor writes.
	ProfileDir string
	// Egress returns the loopback port of the build's egress proxy.
	Egress func() (int, error)
	// Library returns the container library every step loads.
	Library func() (string, error)
}

// seatbeltExecutor runs BuildKit's processes on the host, each confined by a
// Seatbelt profile to its root snapshot and mounts.
type seatbeltExecutor struct {
	opt  execOpt
	mu   sync.Mutex
	runs map[string]*run
}

type run struct {
	root    string
	volumes []seatbelt.Volume
	mounts  []pathmap.Mount
	proxy   int
	done    chan struct{}
}

func newExecutor(opt execOpt) (*seatbeltExecutor, error) {
	if err := os.MkdirAll(opt.ProfileDir, 0o700); err != nil {
		return nil, err
	}
	return &seatbeltExecutor{opt: opt, runs: map[string]*run{}}, nil
}

// mountMap places each BuildKit mount at its destination inside the root as
// a symlink to the directory or file behind it, and undoes it afterwards:
// the root is a snapshot, and the mounts must not end up in its layer.
type mountMap struct {
	links   []string
	hidden  map[string]string
	created []string
	release []func() error
	// mounts presents each mount at its destination through the container
	// library.
	mounts []pathmap.Mount
}

func (m *mountMap) undo() error {
	var errs []error
	for i := len(m.links) - 1; i >= 0; i-- {
		l := m.links[i]
		if err := os.Remove(l); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		if orig, ok := m.hidden[l]; ok {
			if err := os.Rename(orig, l); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for i := len(m.created) - 1; i >= 0; i-- {
		// Only directories made to hold a mount point, and only if empty.
		os.Remove(m.created[i])
	}
	for _, r := range m.release {
		if err := r(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// mountDir returns the directory behind a mountable, and how to release it.
// tmpfs mounts become fresh directories; everything else is a bind mount of
// a snapshot or a file BuildKit prepared.
func mountDir(ctx context.Context, src executor.Mountable, readonly bool, scratch string) (string, func() error, error) {
	ref, err := src.Mount(ctx, readonly)
	if err != nil {
		return "", nil, err
	}
	ms, release, err := ref.Mount()
	if err != nil {
		return "", nil, err
	}
	if len(ms) == 1 && ms[0].Type == "tmpfs" {
		dir, err := os.MkdirTemp(scratch, "tmpfs-")
		if err != nil {
			release()
			return "", nil, err
		}
		return dir, func() error { os.RemoveAll(dir); return release() }, nil
	}
	if len(ms) == 1 && (ms[0].Type == "bind" || ms[0].Type == "rbind") {
		return ms[0].Source, release, nil
	}
	lm := snapshot.LocalMounterWithMounts(ms)
	dir, err := lm.Mount()
	if err != nil {
		release()
		return "", nil, fmt.Errorf("mount %+v: %w", ms, err)
	}
	return dir, func() error { lm.Unmount(); return release() }, nil
}

func (x *seatbeltExecutor) prepare(ctx context.Context, root string, mounts []executor.Mount) (*mountMap, []seatbelt.Volume, error) {
	mm := &mountMap{hidden: map[string]string{}}
	var vols []seatbelt.Volume
	fail := func(err error) (*mountMap, []seatbelt.Volume, error) {
		mm.undo()
		return nil, nil, err
	}
	for _, m := range mounts {
		dir, release, err := mountDir(ctx, m.Src, m.Readonly, x.opt.ProfileDir)
		if err != nil {
			return fail(err)
		}
		mm.release = append(mm.release, release)
		src := dir
		if m.Selector != "" {
			if src, err = rootfs.Join(dir, m.Selector); err != nil {
				return fail(err)
			}
		}
		dest, err := rootfs.Join(root, m.Dest)
		if err != nil {
			return fail(err)
		}
		// Record the directories created to hold the mount point, outermost
		// first, so undo removes them innermost first.
		var missing []string
		for p := filepath.Dir(dest); p != root && len(p) > len(root); p = filepath.Dir(p) {
			if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
				missing = append([]string{p}, missing...)
			}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fail(err)
		}
		mm.created = append(mm.created, missing...)
		if _, err := os.Lstat(dest); err == nil {
			aside := dest + ".fws-hidden"
			if err := os.Rename(dest, aside); err != nil {
				return fail(err)
			}
			mm.hidden[dest] = aside
		}
		// Seatbelt matches resolved paths, and BuildKit makes some sources,
		// such as an SSH mount's socket, under TMPDIR: /var is a symlink to
		// /private/var. A program following the link walks the real path.
		resolved, err := filepath.EvalSymlinks(src)
		if err != nil {
			return fail(err)
		}
		if err := os.Symlink(resolved, dest); err != nil {
			return fail(err)
		}
		mm.links = append(mm.links, dest)
		mm.mounts = append(mm.mounts, pathmap.Mount{Dest: m.Dest, Source: resolved})
		vols = append(vols, seatbelt.Volume{Path: resolved, ReadOnly: m.Readonly})
	}
	return mm, vols, nil
}

// env maps PATH into the root and adds the SANDBOX_* and proxy variables.
func env(meta executor.Meta, root string, proxy int) []string {
	vars := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := vars[k]; !ok {
			order = append(order, k)
		}
		vars[k] = v
	}
	set("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	for _, kv := range meta.Env {
		k, v, _ := strings.Cut(kv, "=")
		set(k, v)
	}
	var mapped []string
	for _, d := range filepath.SplitList(vars["PATH"]) {
		if strings.HasPrefix(d, "/") {
			mapped = append(mapped, filepath.Join(root, d))
		}
	}
	vars["PATH"] = strings.Join(mapped, ":")
	home := filepath.Join(root, "root")
	os.MkdirAll(home, 0o755)
	set("HOME", home)
	set("SANDBOX_ROOT", root)
	set("SANDBOX_IPC_PREFIX", seatbelt.IPCPrefixFor(root))
	set("SANDBOX_HOST", "127.0.0.1")
	if proxy != 0 {
		for _, kv := range egress.Variables(proxy) {
			if _, ok := vars[kv[0]]; !ok {
				set(kv[0], kv[1])
			}
		}
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+vars[k])
	}
	return out
}

func (x *seatbeltExecutor) Run(ctx context.Context, id string, rootMount executor.Mount, mounts []executor.Mount, process executor.ProcessInfo, started chan<- struct{}) (resourcestypes.Recorder, error) {
	root, releaseRoot, err := mountDir(ctx, rootMount.Src, false, x.opt.ProfileDir)
	if err != nil {
		return nil, err
	}
	defer releaseRoot()
	mm, vols, err := x.prepare(ctx, root, mounts)
	if err != nil {
		return nil, err
	}
	defer mm.undo()

	proxy := 0
	if process.Meta.NetMode != pb.NetMode_NONE && x.opt.Egress != nil {
		if proxy, err = x.opt.Egress(); err != nil {
			return nil, err
		}
	}
	r := &run{root: root, volumes: vols, mounts: mm.mounts, proxy: proxy, done: make(chan struct{})}
	if id != "" {
		x.mu.Lock()
		x.runs[id] = r
		x.mu.Unlock()
		defer func() {
			x.mu.Lock()
			delete(x.runs, id)
			x.mu.Unlock()
		}()
	}
	defer close(r.done)
	err = x.exec(ctx, r, process, started)
	// The step ends with its main process: what it started outside its
	// process group goes too, found by the root its environment names.
	if kerr := procs.KillTagged("SANDBOX_ROOT=" + root); kerr != nil && err == nil {
		err = kerr
	}
	return nil, err
}

func (x *seatbeltExecutor) Exec(ctx context.Context, id string, process executor.ProcessInfo) error {
	x.mu.Lock()
	r, ok := x.runs[id]
	x.mu.Unlock()
	if !ok {
		return fmt.Errorf("container %s is not running", id)
	}
	return x.exec(ctx, r, process, nil)
}

func (x *seatbeltExecutor) exec(ctx context.Context, r *run, process executor.ProcessInfo, started chan<- struct{}) error {
	meta := process.Meta
	if len(meta.Args) == 0 {
		return errors.New("no command specified")
	}
	// The step's user is presented through the container library, as for
	// a container's processes.
	id, err := identity.Resolve(r.root, meta.User)
	if err != nil {
		return err
	}
	lib, err := x.opt.Library()
	if err != nil {
		return err
	}
	spec := seatbelt.Spec{Root: r.root, StateRoot: x.opt.StateRoot, Volumes: r.volumes, IPCPrefix: seatbelt.IPCPrefixFor(r.root), Libraries: []string{lib}}
	if r.proxy != 0 {
		spec.ConnectPorts = []int{r.proxy}
	}
	profile, err := os.CreateTemp(x.opt.ProfileDir, "run-*.sb")
	if err != nil {
		return err
	}
	profile.Close()
	defer os.Remove(profile.Name())
	stepEnv := append(env(meta, r.root, r.proxy), id.Env()...)
	if meta.Hostname != "" {
		stepEnv = append(stepEnv, pathmap.HostnameVar+"="+meta.Hostname)
	}
	mv, err := pathmap.MountsVar(r.mounts)
	if err != nil {
		return err
	}
	if mv != "" {
		stepEnv = append(stepEnv, mv)
	}
	rs := runtime.Spec{
		Root: r.root, Argv: meta.Args, Env: stepEnv, WorkDir: meta.Cwd,
		Profile: spec, ProfilePath: profile.Name(), Library: lib,
	}
	var master *os.File
	if meta.Tty {
		var slave *os.File
		if master, slave, err = pty.Open(); err != nil {
			return err
		}
		defer master.Close()
		rs.TTY = slave
		defer slave.Close()
	} else {
		rs.Stdin = process.Stdin
		rs.Stdout, rs.Stderr = writerOrDiscard(process.Stdout), writerOrDiscard(process.Stderr)
	}
	proc, err := runtime.Start(rs)
	if rs.TTY != nil {
		rs.TTY.Close()
	}
	if err != nil {
		return &gatewayapi.ExitError{ExitCode: 127, Err: err}
	}
	if started != nil {
		close(started)
	}
	if master != nil {
		if process.Stdin != nil {
			go io.Copy(master, process.Stdin)
		}
		go io.Copy(writerOrDiscard(process.Stdout), master)
		go func() {
			for ws := range process.Resize {
				pty.Setsize(master, &pty.Winsize{Rows: uint16(ws.Rows), Cols: uint16(ws.Cols)})
			}
		}()
	}
	for wait := true; wait; {
		select {
		case <-proc.Done():
			wait = false
		case sig, ok := <-process.Signal:
			if ok {
				proc.Signal(sig)
			}
		case <-ctx.Done():
			proc.KillAll()
			<-proc.Done()
			wait = false
		}
	}
	proc.KillAll()
	proc.DrainIO(outputDrain)
	if err := ctx.Err(); err != nil {
		return err
	}
	code := proc.ExitCode()
	if code == 0 && meta.ValidExitCodes == nil || slices.Contains(meta.ValidExitCodes, code) {
		return nil
	}
	return &gatewayapi.ExitError{ExitCode: uint32(code)}
}

func writerOrDiscard(w io.WriteCloser) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
