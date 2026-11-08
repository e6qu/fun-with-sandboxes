//go:build e2e && darwin

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcdind "github.com/testcontainers/testcontainers-go/modules/dind"
)

// inDind runs a command in the dind container and returns its output.
func inDind(t *testing.T, name string, args ...string) string {
	t.Helper()
	return mustDocker(t, append([]string{"exec", name}, args...)...)
}

// loadInto saves a host image and loads it into the dind container's
// engine, as Testcontainers' module and CI jobs do.
func loadInto(t *testing.T, name, image string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	save := exec.CommandContext(ctx, "docker", "save", image)
	load := exec.CommandContext(ctx, "docker", "exec", "-i", name, "docker", "load")
	pipe, err := save.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	load.Stdin = pipe
	if err := save.Start(); err != nil {
		t.Fatal(err)
	}
	out, err := load.CombinedOutput()
	if werr := save.Wait(); err != nil || werr != nil {
		t.Fatalf("docker save %s | docker load: %v, %v\n%s", image, err, werr, out)
	}
}

// awaitLog waits for line in the container's log, following the log as
// it grows rather than polling it.
func awaitLog(t *testing.T, name, line string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", name)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	var seen strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(seen.String(), line) {
		n, err := out.Read(buf)
		seen.Write(buf[:n])
		if err != nil {
			t.Fatalf("%s's log ended before %q:\n%s", name, line, seen.String())
		}
	}
}

// A privileged container runs an engine of its own: its docker CLI runs
// containers, Compose projects with their networks, bind mounts of its own
// files and ports published in it, none of which the host engine lists,
// and the engine goes with the container.
func TestDockerInDocker(t *testing.T) {
	const name = "e2e-dind"
	mustDocker(t, "run", "-d", "--privileged", "--name", name, "-e", "DOCKER_TLS_CERTDIR=", "fws/dind:29")
	removed := false
	defer func() {
		if !removed {
			docker(t, "rm", "-f", name)
		}
	}()
	awaitLog(t, name, "API listen on /var/run/docker.sock")
	if out := inDind(t, name, "docker", "version", "--format", "{{.Server.Os}}"); out != "darwin\n" {
		t.Errorf("the nested engine's OS: %q", out)
	}
	loadInto(t, name, "fws/toolbox:1")
	if out := inDind(t, name, "docker", "run", "--rm", "--name", "inner", "fws/toolbox:1", "echo", "inside"); out != "inside\n" {
		t.Errorf("an inner container printed %q", out)
	}
	// A bind mount names the dind container's files.
	inDind(t, name, "sh", "-c", "mkdir -p /work && echo from-dind > /work/note")
	if out := inDind(t, name, "docker", "run", "--rm", "-v", "/work:/w", "fws/toolbox:1", "cat", "/w/note"); out != "from-dind\n" {
		t.Errorf("an inner bind mount read %q", out)
	}
	// Compose in the container: services reach one another by name, and a
	// published port listens in the dind container.
	compose := `services:
  web:
    image: fws/toolbox:1
    ports: ["8080:8080"]
  client:
    image: fws/toolbox:1
    command: ["hold"]
    depends_on:
      web: {condition: service_healthy}
`
	f := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(f, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	mustDocker(t, "cp", f, name+":/work/compose.yaml")
	inDind(t, name, "docker", "compose", "-f", "/work/compose.yaml", "-p", "inner", "up", "-d", "--wait")
	if out := inDind(t, name, "docker", "compose", "-f", "/work/compose.yaml", "-p", "inner", "exec", "-T", "client", "/toolbox", "fetch", "http://web:8080/healthz"); out != "200 OK\n" {
		t.Errorf("the inner client reached web: %q", out)
	}
	if out := inDind(t, name, "wget", "-qO-", "http://localhost:8080/healthz"); out != "ok\n" {
		t.Errorf("the published port in the dind container: %.200q", out)
	}
	// The engines keep their containers apart.
	inner := inDind(t, name, "docker", "ps", "--format", "{{.Names}}")
	if !strings.Contains(inner, "inner-web-1") || strings.Contains(inner, name) {
		t.Errorf("the nested engine lists %q", inner)
	}
	if outer := mustDocker(t, "ps", "-a", "--format", "{{.Names}}"); strings.Contains(outer, "inner-web-1") {
		t.Errorf("the host engine lists the nested engine's containers: %q", outer)
	}
	id := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.Id}}", name))
	mustDocker(t, "rm", "-f", name)
	removed = true
	if _, err := os.Stat(filepath.Join(stateRoot, "run", id[:12])); !os.IsNotExist(err) {
		t.Errorf("the nested engine's sockets outlived the container: %v", err)
	}
}

// With its TLS certificates, the dind container's daemon answers clients
// that hold the client certificate it made, on its published TLS port.
func TestDockerInDockerTLS(t *testing.T) {
	const name = "e2e-dind-tls"
	mustDocker(t, "run", "-d", "--privileged", "--name", name, "-p", "127.0.0.1::2376", "fws/dind:29")
	defer docker(t, "rm", "-f", name)
	awaitLog(t, name, "API listen on [::]:2376")
	certs := t.TempDir()
	mustDocker(t, "cp", name+":/certs/client/.", certs)
	port := strings.TrimSpace(mustDocker(t, "inspect", "-f", `{{(index (index .NetworkSettings.Ports "2376/tcp") 0).HostPort}}`, name))
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "--tlsverify", "--tlscacert", filepath.Join(certs, "ca.pem"),
		"--tlscert", filepath.Join(certs, "cert.pem"), "--tlskey", filepath.Join(certs, "key.pem"),
		"-H", "tcp://localhost:"+port, "info", "--format", "{{.Containers}}").CombinedOutput()
	if err != nil || string(out) != "0\n" {
		t.Errorf("docker info over TLS: %q, %v", out, err)
	}
	out, _ = exec.CommandContext(ctx, "docker", "-H", "tcp://localhost:"+port, "info").CombinedOutput()
	if strings.Contains(string(out), "Containers:") {
		t.Errorf("a client without a certificate got an answer:\n%s", out)
	}
}

// Testcontainers' DinD module starts the image's dockerd, loads an image
// into it and runs a container there.
func TestTestcontainersDinD(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := tcdind.Run(ctx, "fws/dind:29")
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	logsOnFailure(t, c)
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.LoadImage(ctx, "fws/toolbox:1"); err != nil {
		t.Fatal(err)
	}
	daemon := "tcp://" + strings.TrimPrefix(host, "http://")
	out, err := dockerAt(t, daemon, "run", "--rm", "fws/toolbox:1", "echo", "nested")
	if err != nil || out != "nested\n" {
		t.Errorf("a container on the nested engine: %q, %v", out, err)
	}
}

// The privileged container's engine serves Podman's API too: Podman's
// client in a container Podman started runs containers there, and so does
// Docker's, and Podman's client in a container Docker started.
func TestPodmanAndDockerNested(t *testing.T) {
	const name = "e2e-pinp"
	mustPodman(t, "run", "-d", "--privileged", "--name", name, "-e", "DOCKER_TLS_CERTDIR=", "fws/dind:29")
	defer podman(t, "rm", "-f", name)
	awaitLog(t, name, "API listen on /var/run/docker.sock")
	loadInto(t, name, "fws/toolbox:1")
	inner := func(client string) string {
		args := []string{"exec", name, client}
		if client == "podman" {
			args = append(args, "--remote", "--url", "unix:///var/run/docker.sock")
		}
		return mustPodman(t, append(args, "run", "--rm", "fws/toolbox:1", "echo", "nested", client)...)
	}
	if out := inner("podman"); out != "nested podman\n" {
		t.Errorf("Podman in Podman: %q", out)
	}
	if out := inner("docker"); out != "nested docker\n" {
		t.Errorf("Docker in Podman: %q", out)
	}
	if out := mustDocker(t, "exec", name, "podman", "--remote", "--url", "unix:///var/run/docker.sock", "ps", "-a", "--format", "{{.Image}}"); out != "" {
		t.Errorf("Podman in Docker lists removed containers: %q", out)
	}
}
