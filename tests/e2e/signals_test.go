//go:build e2e && darwin

package e2e

import (
	"strings"
	"testing"
)

// A docker exec reaches the container's processes, as `docker exec x kill
// -HUP 1` reloads a service; another container does not.
func TestExecSignalsTheContainersProcesses(t *testing.T) {
	const name = "fws-e2e-exec-signal"
	mustDocker(t, "run", "-d", "--name", name, "fws/toolbox:1", "hold")
	t.Cleanup(func() { docker(t, "rm", "-f", name) })
	pid := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.Pid}}", name))
	if out, err := docker(t, "exec", name, "/toolbox", "signal", pid); err != nil {
		t.Errorf("an exec could not signal its container's process %s: %v\n%s", pid, err, out)
	}
	if _, err := docker(t, "run", "--rm", "fws/toolbox:1", "signal", pid); err == nil {
		t.Errorf("another container signalled process %s", pid)
	}
}

// A container's process listing holds its own processes, its execs among
// them, as a PID namespace does: not another container's, not the host's,
// and not the engine's.
func TestPsListsTheContainersProcesses(t *testing.T) {
	const a, b = "fws-e2e-ps-a", "fws-e2e-ps-b"
	for _, n := range []string{a, b} {
		mustDocker(t, "run", "-d", "--name", n, "fws/toolbox:1", "hold")
		t.Cleanup(func() { docker(t, "rm", "-f", n) })
	}
	pidA := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.Pid}}", a))
	pidB := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.Pid}}", b))
	listed := map[string]string{}
	out := mustDocker(t, "exec", a, "/toolbox", "ps")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		pid, comm, _ := strings.Cut(line, " ")
		listed[pid] = comm
	}
	if _, ok := listed[pidA]; !ok {
		t.Errorf("the container's main process %s is not listed:\n%s", pidA, out)
	}
	if len(listed) != 2 {
		t.Errorf("want the main process and the exec alone:\n%s", out)
	}
	for pid, comm := range listed {
		if pid == pidB || pid == "1" || strings.HasPrefix(comm, "fws") {
			t.Errorf("listed %s %s, which is not the container's:\n%s", pid, comm, out)
		}
	}
}
