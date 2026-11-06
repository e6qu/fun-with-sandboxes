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
	// The main process is the container's pid 1.
	if out, err := docker(t, "exec", name, "/toolbox", "signal", "1"); err != nil {
		t.Errorf("an exec could not signal pid 1: %v\n%s", err, out)
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
	// The main process is listed as pid 1, not by its host pid.
	if _, ok := listed["1"]; !ok {
		t.Errorf("the container's main process is not listed as pid 1:\n%s", out)
	}
	if _, ok := listed[pidA]; ok {
		t.Errorf("the container's main process is listed by its host pid %s:\n%s", pidA, out)
	}
	if len(listed) != 2 {
		t.Errorf("want the main process and the exec alone:\n%s", out)
	}
	for pid, comm := range listed {
		if pid == pidB || (pid == "1" && comm != "toolbox") || strings.HasPrefix(comm, "fws") {
			t.Errorf("listed %s %s, which is not the container's:\n%s", pid, comm, out)
		}
	}
}

// On the host's PID namespace a container lists every host process by its
// host pid, its own main process too, with launchd as pid 1, and signals the
// engine's other containers; its pids limit still counts its own processes
// alone.
func TestPidHost(t *testing.T) {
	const host, other = "fws-e2e-pid-host", "fws-e2e-pid-other"
	mustDocker(t, "run", "-d", "--name", other, "fws/toolbox:1", "hold")
	t.Cleanup(func() { docker(t, "rm", "-f", other) })
	mustDocker(t, "run", "-d", "--name", host, "--pid", "host", "fws/toolbox:1", "hold")
	t.Cleanup(func() { docker(t, "rm", "-f", host) })
	if mode := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.HostConfig.PidMode}}", host)); mode != "host" {
		t.Errorf("inspect reports PID mode %q", mode)
	}
	pidHost := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.Pid}}", host))
	pidOther := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.Pid}}", other))
	listed := map[string]string{}
	out := mustDocker(t, "exec", host, "/toolbox", "ps")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		pid, name, _ := strings.Cut(strings.TrimSpace(line), " ")
		listed[pid] = name
	}
	if listed[pidHost] != "toolbox" || listed[pidOther] != "toolbox" || listed["1"] != "launchd" {
		t.Errorf("want both main processes by their host pids, %s and %s, and launchd as pid 1:\n%s", pidHost, pidOther, out)
	}
	if out, err := docker(t, "exec", host, "/toolbox", "signal", pidOther); err != nil {
		t.Errorf("a container on the host's PID namespace could not signal process %s: %v\n%s", pidOther, err, out)
	}

	const limited = "fws-e2e-pid-host-limit"
	mustDocker(t, "run", "-d", "--name", limited, "--pid", "host", "--pids-limit", "6", "fws/toolbox:1", "forkmany", "20")
	t.Cleanup(func() { docker(t, "rm", "-f", limited) })
	waitLog(t, limited, "fork 6 failed")

	if _, err := docker(t, "run", "--rm", "--pid", "container:"+other, "fws/toolbox:1", "echo"); err == nil || !strings.Contains(err.Error(), "is not supported") {
		t.Errorf("--pid container: %v", err)
	}
}
