package engine

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"

	"github.com/e6qu/fun-with-sandboxes/internal/pathmap"
	"github.com/e6qu/fun-with-sandboxes/internal/pidns"
)

// pidnsFile is the file of the PID namespace whose pid 1 is c's main
// process (package pidns), in c's state, which no container writes.
func (e *Engine) pidnsFile(c *Container) string { return filepath.Join(c.dir, "pidns") }

// procsSocket is where c's shim answers its library about its PID
// namespace.
func (e *Engine) procsSocket(c *Container) string {
	return filepath.Join(e.cfg.Root, "run", c.rec.ID[:12]+".procs")
}

// pidOwner is the container whose main process is c's PID namespace's pid
// 1: c itself, or for --pid container:<name>, that container's owner.
func (e *Engine) pidOwner(c *Container) (*Container, error) {
	for range 16 {
		ref, ok := strings.CutPrefix(string(c.rec.HostConfig.PidMode), "container:")
		if !ok {
			return c, nil
		}
		o, err := e.Container(ref)
		if err != nil {
			return nil, err
		}
		c = o
	}
	return nil, invalid("PID mode: containers join one another's PID namespaces in a cycle")
}

// joinPIDNamespace enters c in its PID namespace's file as it starts: a
// namespace of its own, or the running owner's, which it joins. It returns
// the namespace's file and, for a container that joins another's, the
// host pid of the namespace's pid 1.
func (e *Engine) joinPIDNamespace(c *Container) (string, int, error) {
	owner, err := e.pidOwner(c)
	if err != nil {
		return "", 0, err
	}
	if owner.rec.HostConfig.PidMode.IsHost() {
		return "", 0, nil
	}
	file := e.pidnsFile(owner)
	e.pidnsMu.Lock()
	defer e.pidnsMu.Unlock()
	if owner == c {
		return file, 0, pidns.Write(file, []string{c.tag()})
	}
	owner.mu.Lock()
	pid := owner.rec.State.Pid
	running := owner.rec.State.Status == container.StateRunning
	owner.mu.Unlock()
	if !running || pid == 0 {
		return "", 0, conflict("cannot join PID of a non running container: %s", owner.rec.ID)
	}
	return file, pid, pidns.Join(file, c.tag())
}

// leavePIDNamespace takes c, which joined another's PID namespace, out of
// its file as it ends.
func (e *Engine) leavePIDNamespace(c *Container) error {
	if !strings.HasPrefix(string(c.rec.HostConfig.PidMode), "container:") {
		return nil
	}
	owner, err := e.pidOwner(c)
	if err != nil {
		return nil
	}
	e.pidnsMu.Lock()
	defer e.pidnsMu.Unlock()
	return pidns.Leave(e.pidnsFile(owner), c.tag())
}

// pidnsEnv is what the container library reads of c's PID namespace: its
// file, the socket c's shim answers on, and for a container on another's
// namespace, the host pid of its pid 1.
func (e *Engine) pidnsEnv(c *Container) ([]string, error) {
	owner, err := e.pidOwner(c)
	if err != nil {
		return nil, err
	}
	if owner.rec.HostConfig.PidMode.IsHost() {
		return []string{pathmap.PidNSVar + "=host"}, nil
	}
	env := []string{pathmap.PidNSFileVar + "=" + e.pidnsFile(owner), pathmap.ProcsVar + "=" + e.procsSocket(c)}
	if owner != c {
		owner.mu.Lock()
		pid := owner.rec.State.Pid
		owner.mu.Unlock()
		env = append(env, pathmap.PidNSVar+"="+strconv.Itoa(pid))
	}
	return env, nil
}
