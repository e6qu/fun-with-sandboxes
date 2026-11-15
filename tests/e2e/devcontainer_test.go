//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
)

// devcontainer runs the Dev Containers CLI, which drives the engine through
// the Docker CLI and DOCKER_HOST as it drives Docker.
func devcontainer(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := stacksOnTimeout(exec.CommandContext(ctx, "devcontainer", args...))
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("devcontainer %s: %v\n%s\n%s", strings.Join(args, " "), err, out.String(), errb.String())
	}
	return out.String(), nil
}

// upResult is what devcontainer up prints once the container is ready.
type upResult struct {
	Outcome               string `json:"outcome"`
	ContainerID           string `json:"containerId"`
	RemoteUser            string `json:"remoteUser"`
	RemoteWorkspaceFolder string `json:"remoteWorkspaceFolder"`
	Message               string `json:"message"`
}

// devcontainerUp brings up the dev container of the workspace made of
// files, and removes it when the test ends.
func devcontainerUp(t *testing.T, files map[string]string) (string, upResult) {
	t.Helper()
	// The workspace is a directory under the user's home, as a project's
	// is: one of the trees a container's profile denies but for its mounts.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, "fws-e2e-devcontainer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	out, err := devcontainer(t, "up", "--workspace-folder", dir)
	if err != nil {
		t.Fatal(err)
	}
	// The result is the last line of standard output.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var res upResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil {
		t.Fatalf("devcontainer up printed %q: %v", out, err)
	}
	if res.ContainerID != "" {
		t.Cleanup(func() { docker(t, "rm", "-f", "-v", res.ContainerID) })
	}
	if res.Outcome != "success" {
		t.Fatalf("devcontainer up: %+v", res)
	}
	return dir, res
}

// The Dev Containers CLI brings up a devcontainer.json on a darwin image:
// the workspace is mounted, the container's and the remote environment
// reach commands, the lifecycle commands run once in order, and a second
// up finds the container it made.
func TestDevcontainerUp(t *testing.T) {
	dir, res := devcontainerUp(t, map[string]string{
		"README": "from the workspace\n",
		".devcontainer/devcontainer.json": `{
  "name": "fws e2e",
  "image": "fws/base:1",
  "containerEnv": {"FROM_CONTAINER": "container"},
  "remoteEnv": {"FROM_REMOTE": "remote"},
  "onCreateCommand": "echo on-create >> /tmp/lifecycle",
  "postCreateCommand": "echo post-create >> /tmp/lifecycle",
  "postStartCommand": {"start": "echo post-start >> /tmp/lifecycle"}
}
`,
	})
	want := "/workspaces/" + filepath.Base(dir)
	if res.RemoteWorkspaceFolder != want || res.RemoteUser != "root" {
		t.Errorf("devcontainer up: %+v, want the workspace at %s for root", res, want)
	}
	exec := func(script string) string {
		t.Helper()
		out, err := devcontainer(t, "exec", "--workspace-folder", dir, "sh", "-c", script)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := exec("pwd; cat README; echo $FROM_CONTAINER $FROM_REMOTE"); out != want+"\nfrom the workspace\ncontainer remote\n" {
		t.Errorf("a command in the dev container printed %q", out)
	}
	if out := exec("cat /tmp/lifecycle"); out != "on-create\npost-create\npost-start\n" {
		t.Errorf("the lifecycle commands left %q", out)
	}
	// The workspace is the host's directory, both ways.
	exec("echo from the container > made-inside")
	if b, err := os.ReadFile(filepath.Join(dir, "made-inside")); err != nil || string(b) != "from the container\n" {
		t.Errorf("a file the container wrote in the workspace: %q, %v", b, err)
	}

	// A second up finds the container and creates nothing again.
	out, err := devcontainer(t, "up", "--workspace-folder", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, res.ContainerID) {
		t.Errorf("a second up made another container: %s", out)
	}
	if out := exec("cat /tmp/lifecycle"); out != "on-create\npost-create\npost-start\n" {
		t.Errorf("a second up ran lifecycle commands again: %q", out)
	}
}

// A dev container built from a Dockerfile with a feature of the
// workspace's own: the CLI builds the image through buildx, with the
// feature's files as a named build context, and the feature's install
// script and options take effect in the container.
func TestDevcontainerDockerfileAndFeature(t *testing.T) {
	dir, _ := devcontainerUp(t, map[string]string{
		".devcontainer/Dockerfile": "FROM fws/base:1\nRUN echo built > /etc/fws-e2e-built\n",
		".devcontainer/devcontainer.json": `{
  "build": {"dockerfile": "Dockerfile"},
  "features": {"./greeting": {"word": "hello"}}
}
`,
		".devcontainer/greeting/devcontainer-feature.json": `{
  "id": "greeting",
  "version": "1.0.0",
  "name": "Greeting",
  "options": {"word": {"type": "string", "default": "hi"}},
  "containerEnv": {"GREETING_FEATURE": "installed"}
}
`,
		".devcontainer/greeting/install.sh": "#!/bin/sh\nset -e\necho \"$WORD from a feature\" > /etc/fws-e2e-greeting\n",
	})
	out, err := devcontainer(t, "exec", "--workspace-folder", dir, "sh", "-c", "cat /etc/fws-e2e-built /etc/fws-e2e-greeting; echo $GREETING_FEATURE")
	if err != nil {
		t.Fatal(err)
	}
	if out != "built\nhello from a feature\ninstalled\n" {
		t.Errorf("the built dev container printed %q", out)
	}
}

// A feature published to a registry as an OCI artifact: the CLI pushes it
// with features publish, and up fetches it by reference and installs it.
func TestDevcontainerRegistryFeature(t *testing.T) {
	reg := httptest.NewServer(registry.New())
	defer reg.Close()
	// The CLI speaks plain HTTP only to a registry named localhost.
	host := "localhost" + reg.URL[strings.LastIndex(reg.URL, ":"):]

	src := filepath.Join(t.TempDir(), "src", "motto")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"devcontainer-feature.json": `{
  "id": "motto",
  "version": "1.2.3",
  "name": "Motto",
  "options": {"text": {"type": "string", "default": "none"}}
}
`,
		"install.sh": "#!/bin/sh\nset -e\necho \"$TEXT\" > /etc/fws-e2e-motto\n",
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := devcontainer(t, "features", "publish", "--registry", host, "--namespace", "e2e/features", filepath.Dir(src)); err != nil {
		t.Fatal(err)
	}

	dir, _ := devcontainerUp(t, map[string]string{
		".devcontainer/devcontainer.json": `{
  "image": "fws/base:1",
  "features": {"` + host + `/e2e/features/motto:1": {"text": "from a registry"}}
}
`,
	})
	out, err := devcontainer(t, "exec", "--workspace-folder", dir, "cat", "/etc/fws-e2e-motto")
	if err != nil {
		t.Fatal(err)
	}
	if out != "from a registry\n" {
		t.Errorf("the feature from the registry left %q", out)
	}
}

// appPort publishes a port of the dev container on the host, and the
// container's server answers there.
func TestDevcontainerAppPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	devcontainerUp(t, map[string]string{
		"index.txt": "served from the workspace\n",
		".devcontainer/devcontainer.json": `{
  "image": "fws/python:1",
  "appPort": ["127.0.0.1:` + port + `:8000"],
  "postStartCommand": "nohup python3 -m http.server 8000 > /tmp/server.log 2>&1 &"
}
`,
	})
	var body string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		res, err := http.Get("http://127.0.0.1:" + port + "/index.txt")
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		body = string(b)
		break
	}
	if body != "served from the workspace\n" {
		t.Errorf("the dev container's published port answered %q", body)
	}
}

// A dev container that is a service of a Compose project: the CLI brings
// the project up with an override file of its own, runs commands in the
// named service, and the service reaches its neighbour by name.
func TestDevcontainerCompose(t *testing.T) {
	dir, res := devcontainerUp(t, map[string]string{
		"index.txt": "from the neighbour\n",
		".devcontainer/compose.yaml": `services:
  dev:
    image: fws/python:1
    command: ["python3", "-c", "import time; time.sleep(600)"]
    volumes:
      - ..:/workspace
  web:
    image: fws/python:1
    working_dir: /srv
    command: ["python3", "-m", "http.server", "8000"]
    volumes:
      - ..:/srv
`,
		".devcontainer/devcontainer.json": `{
  "dockerComposeFile": "compose.yaml",
  "service": "dev",
  "workspaceFolder": "/workspace",
  "postCreateCommand": "echo created > /tmp/created"
}
`,
	})
	// The CLI names the project after the folder that holds .devcontainer.
	project := "com.docker.compose.project=" + strings.ToLower(filepath.Base(dir)) + "_devcontainer"
	t.Cleanup(func() {
		if ids, _ := docker(t, "ps", "-aq", "--filter", "label="+project); strings.TrimSpace(ids) != "" {
			docker(t, append([]string{"rm", "-f", "-v"}, strings.Fields(ids)...)...)
		}
		if ids, _ := docker(t, "network", "ls", "-q", "--filter", "label="+project); strings.TrimSpace(ids) != "" {
			docker(t, append([]string{"network", "rm"}, strings.Fields(ids)...)...)
		}
	})
	if res.RemoteWorkspaceFolder != "/workspace" {
		t.Errorf("devcontainer up: %+v", res)
	}
	script := `import urllib.request, time
for i in range(100):
    try:
        print(urllib.request.urlopen("http://web:8000/index.txt").read().decode(), end="")
        break
    except OSError:
        time.sleep(0.2)
print(open("/tmp/created").read(), end="")`
	out, err := devcontainer(t, "exec", "--workspace-folder", dir, "python3", "-c", script)
	if err != nil {
		t.Fatal(err)
	}
	if out != "from the neighbour\ncreated\n" {
		t.Errorf("the Compose dev container printed %q", out)
	}
}
