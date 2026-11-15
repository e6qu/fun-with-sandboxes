//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
