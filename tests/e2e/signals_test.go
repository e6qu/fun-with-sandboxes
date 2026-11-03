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
