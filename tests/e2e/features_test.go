//go:build e2e && darwin

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// hostAddress returns a non-loopback IPv4 address of this machine.
func hostAddress(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	t.Fatal("this machine has no non-loopback IPv4 address")
	return ""
}

func TestEgressReachesTheInternetButNotTheHost(t *testing.T) {
	// A service on the host's own network address: reachable from the
	// internet's point of view, but it is the host.
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "host data") })}
	go srv.Serve(l)
	defer srv.Close()
	hostURL := fmt.Sprintf("http://%s:%d/", hostAddress(t), l.Addr().(*net.TCPAddr).Port)
	if res, err := http.Get(hostURL); err != nil {
		t.Fatalf("the host service is not reachable from the host itself: %v", err)
	} else {
		res.Body.Close()
	}

	run := func(args ...string) error {
		_, err := docker(t, append([]string{"run", "--rm"}, args...)...)
		return err
	}
	if err := run("fws/toolbox:1", "fetch", "https://github.com/"); err != nil {
		t.Errorf("container could not reach the internet through its proxy: %v", err)
	}
	if err := run("fws/toolbox:1", "fetch", hostURL); err == nil {
		t.Errorf("container reached the host at %s through the proxy", hostURL)
	}
	if err := run("fws/toolbox:1", "dial", "github.com:443"); err == nil {
		t.Error("container dialled the internet directly, bypassing the proxy")
	}
	if err := run("--network", "none", "fws/toolbox:1", "fetch", "https://github.com/"); err == nil {
		t.Error("a container with network none reached the internet")
	}
	mustDocker(t, "network", "create", "--internal", "e2e-internal")
	defer docker(t, "network", "rm", "e2e-internal")
	if err := run("--network", "e2e-internal", "fws/toolbox:1", "fetch", "https://github.com/"); err == nil {
		t.Error("a container on an internal network reached the internet")
	}
}

func TestDockerCp(t *testing.T) {
	mustDocker(t, "run", "-d", "--name", "cptest", "fws/toolbox:1")
	defer docker(t, "rm", "-f", "cptest")
	dir := t.TempDir()
	src := filepath.Join(dir, "hello.txt")
	os.WriteFile(src, []byte("copied in\n"), 0o644)

	mustDocker(t, "cp", src, "cptest:/tmp/")
	if out := mustDocker(t, "exec", "cptest", "/toolbox", "cat", "/tmp/hello.txt"); out != "copied in\n" {
		t.Fatalf("file inside the container holds %q", out)
	}
	back := filepath.Join(dir, "back.txt")
	mustDocker(t, "cp", "cptest:/tmp/hello.txt", back)
	if b, _ := os.ReadFile(back); string(b) != "copied in\n" {
		t.Fatalf("copied back %q", b)
	}
	mustDocker(t, "cp", "cptest:/tmp", filepath.Join(dir, "tree"))
	if b, _ := os.ReadFile(filepath.Join(dir, "tree", "hello.txt")); string(b) != "copied in\n" {
		t.Fatalf("directory copy lacks the file: %q", b)
	}
	if _, err := docker(t, "cp", "cptest:/no/such/file", dir); err == nil || !strings.Contains(err.Error(), "Could not find the file") {
		t.Fatalf("copying a missing path: %v", err)
	}
}

func TestTestcontainers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// Ryuk, Testcontainers' reaper, ships only as a Linux image; the test
	// terminates its own container instead.
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	redis, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "fws/redis:7",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections"),
			Files: []testcontainers.ContainerFile{{
				Reader: strings.NewReader("seeded\n"), ContainerFilePath: "/tmp/seed.txt", FileMode: 0o644,
			}},
		},
		Started: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer redis.Terminate(context.Background())

	endpoint, err := redis.PortEndpoint(ctx, "6379/tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "PING\r\n")
	if line, _ := bufio.NewReader(c).ReadString('\n'); strings.TrimSpace(line) != "+PONG" {
		t.Fatalf("redis at %s answered %q", endpoint, line)
	}

	code, out, err := redis.Exec(ctx, []string{"/entry", "health"})
	if err != nil || code != 0 {
		b, _ := io.ReadAll(out)
		t.Fatalf("exec health: code %d, err %v, output %s", code, err, b)
	}
	rc, err := redis.CopyFileFromContainer(ctx, "/tmp/seed.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(b, []byte("seeded\n")) {
		t.Fatalf("file copied in at creation reads back %q", b)
	}
}
