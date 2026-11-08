//go:build e2e && darwin

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// podman runs Podman's remote client against the engine, which serves
// Podman's API beside Docker's on its socket.
func podman(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "podman", append([]string{"--remote"}, args...)...)
	cmd.Env = append(os.Environ(), "CONTAINER_HOST="+dockerHost)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustPodman(t *testing.T, args ...string) string {
	t.Helper()
	out, err := podman(t, args...)
	if err != nil {
		t.Fatalf("podman %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// podman --remote runs, lists, logs, execs into and removes the engine's
// containers, on networks, with volumes and in pods, as it does Podman's.
func TestPodmanRemote(t *testing.T) {
	if out := mustPodman(t, "version", "--format", "{{.Server.APIVersion}}"); !strings.HasPrefix(out, "5.") {
		t.Errorf("the server's API version: %q", out)
	}
	if out := mustPodman(t, "run", "--rm", "fws/python:3.14", "python3", "-c", "print('from podman')"); out != "from podman\n" {
		t.Errorf("podman run printed %q", out)
	}
	if _, err := podman(t, "run", "--rm", "fws/python:3.14", "python3", "-c", "raise SystemExit(3)"); err == nil {
		t.Error("a failing command's exit status was lost")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Errorf("podman run of a command that exits 3: %v", err)
	}

	mustPodman(t, "network", "create", "e2e-podnet")
	defer podman(t, "network", "rm", "-f", "e2e-podnet")
	mustPodman(t, "volume", "create", "e2e-podvol")
	defer podman(t, "volume", "rm", "-f", "e2e-podvol")
	mustPodman(t, "pod", "create", "--infra=false", "--share=", "--name", "e2e-pod")
	defer podman(t, "pod", "rm", "-f", "e2e-pod")
	mustPodman(t, "run", "-d", "--name", "e2e-pweb", "--pod", "e2e-pod", "--network", "e2e-podnet", "-v", "e2e-podvol:/data",
		"--health-cmd", "python3 -c \"import urllib.request; urllib.request.urlopen('http://localhost:8000')\"", "--health-interval", "1s",
		"fws/python:3.14", "python3", "-c", "open('/data/note', 'w').write('kept'); print('serving', flush=True); import http.server; http.server.test(http.server.SimpleHTTPRequestHandler, port=8000, bind='0.0.0.0')")
	if out := mustPodman(t, "wait", "--condition", "healthy", "e2e-pweb"); out != "-1\n" {
		t.Errorf("podman wait for health: %q", out)
	}
	if out := mustPodman(t, "ps", "--format", "{{.Names}} {{.PodName}}"); !strings.Contains(out, "e2e-pweb e2e-pod") {
		t.Errorf("podman ps: %q", out)
	}
	if out := mustPodman(t, "pod", "ps", "--format", "{{.Name}} {{.NumberOfContainers}}"); !strings.Contains(out, "e2e-pod 1") {
		t.Errorf("podman pod ps: %q", out)
	}
	if out := mustPodman(t, "exec", "e2e-pweb", "python3", "-c", "print(open('/data/note').read())"); out != "kept\n" {
		t.Errorf("podman exec read %q", out)
	}
	if out := mustPodman(t, "run", "--rm", "--network", "e2e-podnet", "fws/python:3.14", "python3", "-c",
		"import urllib.request; print(urllib.request.urlopen('http://e2e-pweb:8000').status)"); out != "200\n" {
		t.Errorf("a container on the network reached the pod's by name: %q", out)
	}
	if out := mustPodman(t, "logs", "e2e-pweb"); !strings.Contains(out, "serving") {
		t.Errorf("podman logs: %q", out)
	}
	if out := mustPodman(t, "inspect", "--format", "{{.State.Status}} {{.Pod}}", "e2e-pweb"); !strings.HasPrefix(out, "running ") || len(strings.TrimSpace(out)) < len("running x") {
		t.Errorf("podman inspect: %q", out)
	}
	mustPodman(t, "stop", "-t", "2", "e2e-pweb")
	mustPodman(t, "rm", "e2e-pweb")
	if out := mustPodman(t, "ps", "-a", "--format", "{{.Names}}"); strings.Contains(out, "e2e-pweb") {
		t.Errorf("a removed container listed: %q", out)
	}
	if _, err := podman(t, "pod", "create", "--name", "e2e-infra"); err == nil {
		podman(t, "pod", "rm", "-f", "e2e-infra")
		t.Error("a pod with an infra container was made")
	}
}

// podman-compose brings a project up through podman, in a pod of its own,
// and its services reach one another by name.
func TestPodmanCompose(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(`services:
  web:
    image: fws/python:3.14
    command: ["python3", "-m", "http.server", "8000"]
    healthcheck:
      test: ["CMD", "python3", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:8000')"]
      interval: 1s
  client:
    image: fws/python:3.14
    command: ["python3", "-c", "import time; time.sleep(600)"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	compose := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "podman-compose", append([]string{"-p", "e2epc", "-f", filepath.Join(dir, "compose.yaml")}, args...)...)
		cmd.Env = append(os.Environ(), "CONTAINER_HOST="+dockerHost)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("podman-compose %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	defer compose("down")
	compose("up", "-d")
	mustPodman(t, "wait", "--condition", "healthy", "e2epc_web_1")
	out := compose("exec", "client", "python3", "-c", "import urllib.request; print(urllib.request.urlopen('http://web:8000').status)")
	if !strings.Contains(out, "200") {
		t.Errorf("the client reached web: %q", out)
	}
}
