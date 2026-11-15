//go:build e2e && darwin

package e2e

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A block I/O weight picks the disk I/O tier of a container's processes,
// and an update reaches every process the container starts after it,
// execs and a running process's children alike, while a process that
// runs on keeps its tier until the container restarts.
func TestBlkioWeightUpdate(t *testing.T) {
	const name = "fws-e2e-blkio"
	mustDocker(t, "run", "-d", "--name", name, "--blkio-weight", "50", "fws/toolbox:1", "iopolicy", "watch")
	defer docker(t, "rm", "-f", name)
	// IOPOL_THROTTLE is 3, IOPOL_UTILITY 4.
	waitLog(t, name, "self=3 child=3")
	if out := mustDocker(t, "exec", name, "/toolbox", "iopolicy"); out != "iopolicy 3\n" {
		t.Fatalf("an exec at weight 50: %q", out)
	}
	mustDocker(t, "update", "--blkio-weight", "300", name)
	if s := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.HostConfig.BlkioWeight}}", name)); s != "300" {
		t.Fatalf("inspect reports a block I/O weight of %s", s)
	}
	if out := mustDocker(t, "exec", name, "/toolbox", "iopolicy"); out != "iopolicy 4\n" {
		t.Fatalf("an exec after the update to 300: %q", out)
	}
	waitLog(t, name, "self=3 child=4")
	mustDocker(t, "restart", "-t", "0", name)
	waitLog(t, name, "self=4 child=4")

	// A container started without a weight takes one from an update.
	const plain = "fws-e2e-blkio-plain"
	mustDocker(t, "run", "-d", "--name", plain, "fws/toolbox:1", "hold")
	defer docker(t, "rm", "-f", plain)
	mustDocker(t, "update", "--blkio-weight", "10", plain)
	if out := mustDocker(t, "exec", plain, "/toolbox", "iopolicy"); out != "iopolicy 3\n" {
		t.Fatalf("an exec after the update to 10: %q", out)
	}
}

// waitInnerLog waits for text in the log of the dind container name's
// inner container inner.
func waitInnerLog(t *testing.T, name, inner, text string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		out, _ := docker(t, "exec", name, "docker", "logs", inner)
		if strings.Contains(out, text) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's log in %s holds no %q:\n%s", inner, name, text, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// udpAsk sends "ping" to UDP address addr, once a second until an answer
// comes, for ten seconds, and returns the answer.
func udpAsk(t *testing.T, addr string) string {
	t.Helper()
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 1500)
	for range 10 {
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		next := time.Now().Add(time.Second)
		c.SetReadDeadline(next)
		if n, err := c.Read(buf); err == nil {
			return string(buf[:n])
		}
		time.Sleep(time.Until(next))
	}
	return ""
}

// copyToolbox copies the toolbox image's binary into container name.
func copyToolbox(t *testing.T, name, to string) {
	t.Helper()
	box := name + "-toolbox"
	mustDocker(t, "create", "--name", box, "fws/toolbox:1", "hold")
	defer docker(t, "rm", "-f", box)
	local := filepath.Join(t.TempDir(), "toolbox")
	mustDocker(t, "cp", box+":/toolbox", local)
	mustDocker(t, "cp", local, name+":"+to)
}

// A dind container's engine publishes UDP ports in it as it publishes TCP
// ones: on a port the dind container publishes on the host, which the
// host's clients reach, and on a port without a host port, which the dind
// container's processes reach.
func TestDockerInDockerPublishesUDP(t *testing.T) {
	const name = "e2e-dind-udp"
	mustDocker(t, "run", "-d", "--privileged", "--name", name, "-e", "DOCKER_TLS_CERTDIR=", "-p", "127.0.0.1::5353/udp", "fws/dind:29")
	defer docker(t, "rm", "-f", name)
	awaitLog(t, name, "API listen on /var/run/docker.sock")
	loadInto(t, name, "fws/toolbox:1")

	inDind(t, name, "docker", "run", "-d", "--name", "dns", "-p", "5353:5353/udp", "fws/toolbox:1", "udpecho", "5353")
	waitInnerLog(t, name, "dns", "listening on udp")
	addr := strings.TrimSpace(mustDocker(t, "port", name, "5353/udp"))
	if got := udpAsk(t, addr); got != "echo:ping" {
		t.Fatalf("the host's client of %s got %q", addr, got)
	}

	inDind(t, name, "docker", "run", "-d", "--name", "echo", "-p", "6363:6363/udp", "fws/toolbox:1", "udpecho", "6363")
	waitInnerLog(t, name, "echo", "listening on udp")
	copyToolbox(t, name, "/usr/local/bin/toolbox")
	if out := inDind(t, name, "/usr/local/bin/toolbox", "udpask", "127.0.0.1:6363"); out != "echo:ping\n" {
		t.Fatalf("the dind container's client got %q", out)
	}
	// The engine closes the port's socket with its container.
	inDind(t, name, "docker", "rm", "-f", "echo")
	if out, err := docker(t, "exec", name, "/usr/local/bin/toolbox", "udpask", "127.0.0.1:6363"); err == nil {
		t.Fatalf("a removed container's port answered: %q", out)
	}
}
