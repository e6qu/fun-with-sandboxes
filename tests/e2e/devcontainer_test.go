//go:build e2e && darwin

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	if args[0] == "exec" {
		// Its log, which a failure prints, tells what environment the CLI
		// probed in the container and ran the command with.
		args = append([]string{"exec", "--log-level", "trace"}, args[1:]...)
	}
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
	return dir, devcontainerUpIn(t, dir)
}

// devcontainerUpIn runs devcontainer up in the workspace dir with args, as
// VS Code's Reopen in Container and Rebuild Container do, and removes the
// container it reports when the test ends.
func devcontainerUpIn(t *testing.T, dir string, args ...string) upResult {
	t.Helper()
	out, err := devcontainer(t, append([]string{"up", "--workspace-folder", dir}, args...)...)
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
	return res
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
			// What the CLI's probe of the user's environment saw.
			plain, _ := docker(t, "exec", res.ContainerID, "/bin/sh", "-c", "printenv")
			login, lerr := docker(t, "exec", res.ContainerID, "/bin/sh", "-c", "grep root /etc/passwd; bash -lic printenv")
			t.Fatalf("%v\nthe container's environment:\n%s\na login shell's:\n%s%v", err, plain, login, lerr)
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
	port := fmt.Sprint(freePort(t))
	// The image's own command serves the workspace: overrideCommand keeps
	// it, where the CLI would otherwise run a shell loop that sleeps.
	devcontainerUp(t, map[string]string{
		"index.txt":                "served from the workspace\n",
		".devcontainer/Dockerfile": "FROM fws/python:3.14\nCMD [\"python3\", \"-m\", \"http.server\", \"8000\", \"--directory\", \"/srv\"]\n",
		".devcontainer/devcontainer.json": `{
  "build": {"dockerfile": "Dockerfile"},
  "overrideCommand": false,
  "workspaceMount": "source=${localWorkspaceFolder},target=/srv,type=bind",
  "workspaceFolder": "/srv",
  "appPort": ["127.0.0.1:` + port + `:8000"]
}
`,
	})
	var body string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		res, err := pollClient.Get("http://127.0.0.1:" + port + "/index.txt")
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
// the project up with an override file of its own, starting the services
// runServices names alone, runs commands in the named service, and the
// service reaches its neighbour by name.
func TestDevcontainerCompose(t *testing.T) {
	dir, res := devcontainerUp(t, map[string]string{
		"index.txt": "from the neighbour\n",
		".devcontainer/compose.yaml": `services:
  dev:
    image: fws/base:1
    command: ["sleep", "600"]
    volumes:
      - ..:/workspace
  web:
    image: fws/python:3.14
    working_dir: /srv
    command: ["python3", "-m", "http.server", "8000"]
    volumes:
      - ..:/srv
  idle:
    image: fws/base:1
    command: ["sleep", "600"]
`,
		".devcontainer/devcontainer.json": `{
  "dockerComposeFile": "compose.yaml",
  "service": "dev",
  "runServices": ["dev", "web"],
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
	// The CLI keeps its record of the lifecycle commands it ran with the
	// container's own mkdir and cat, so the service's image is one with
	// the usual commands.
	out, err := devcontainer(t, "exec", "--workspace-folder", dir, "sh", "-c",
		"curl -fsS --retry 30 --retry-delay 1 --retry-all-errors http://web:8000/index.txt && cat /tmp/created")
	if err != nil {
		t.Fatal(err)
	}
	if out != "from the neighbour\ncreated\n" {
		t.Errorf("the Compose dev container printed %q", out)
	}
	if ids, _ := docker(t, "ps", "-aq", "--filter", "label="+project, "--filter", "label=com.docker.compose.service=idle"); strings.TrimSpace(ids) != "" {
		t.Errorf("a service runServices leaves out has containers: %s", ids)
	}
}

// The lifecycle commands run where and when the Dev Containers CLI runs
// them, as VS Code's Reopen in Container and Rebuild Container do:
// initializeCommand on the host before each up; the creation commands
// once per container, in order, in their string, array and parallel
// object forms; postStartCommand at each start, and postAttachCommand at
// each attach. A stopped container starts again with its files, a rebuild
// makes a new container, and a named volume of mounts keeps what the
// first one wrote. devcontainer exec passes the command's exit status and
// streams through, with the environment --remote-env adds.
func TestDevcontainerLifecycle(t *testing.T) {
	const volume = "fws-e2e-devcontainer-cache"
	docker(t, "volume", "rm", "-f", volume)
	t.Cleanup(func() { docker(t, "volume", "rm", "-f", volume) })
	dir, first := devcontainerUp(t, map[string]string{
		".devcontainer/devcontainer.json": `{
  "image": "fws/base:1",
  "initializeCommand": "echo initialized >> '${localWorkspaceFolder}/initialized'",
  "mounts": ["source=` + volume + `,target=/cache,type=volume"],
  "onCreateCommand": "echo on-create >> /tmp/lifecycle; echo created >> /cache/creations",
  "updateContentCommand": ["/bin/sh", "-c", "echo update-content >> /tmp/lifecycle"],
  "postCreateCommand": {
    "logged": "echo post-create >> /tmp/lifecycle",
    "beside": ["sh", "-c", "echo beside > /tmp/post-create-beside"]
  },
  "postStartCommand": "echo post-start >> /tmp/lifecycle",
  "postAttachCommand": "echo post-attach >> /tmp/lifecycle"
}
`,
	})
	read := func(want string) {
		t.Helper()
		out, err := devcontainer(t, "exec", "--workspace-folder", dir, "sh", "-c", "cat /tmp/lifecycle /tmp/post-create-beside /cache/creations initialized")
		if err != nil {
			t.Fatal(err)
		}
		if out != want {
			t.Errorf("the lifecycle commands left %q, want %q", out, want)
		}
	}
	read("on-create\nupdate-content\npost-create\npost-start\npost-attach\nbeside\ncreated\ninitialized\n")

	// The command's own status and streams, apart from the CLI's log.
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "devcontainer", "exec", "--workspace-folder", dir, "--remote-env", "FROM_FLAG=flag",
		"sh", "-c", "echo $FROM_FLAG; echo to-stderr >&2; exit 3")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if cmd.ProcessState == nil {
		t.Fatalf("devcontainer exec: %v", err)
	}
	if code := cmd.ProcessState.ExitCode(); code != 3 || stdout.String() != "flag\n" || !strings.Contains(stderr.String(), "to-stderr") {
		t.Errorf("devcontainer exec of a command that exits 3: status %d (%v), stdout %q, stderr %q", code, err, stdout.String(), stderr.String())
	}

	// Reopening a stopped container starts it again: postStartCommand runs
	// for the new start, postAttachCommand for the new attach, nothing else.
	mustDocker(t, "stop", first.ContainerID)
	if again := devcontainerUpIn(t, dir); again.ContainerID != first.ContainerID {
		t.Errorf("reopening a stopped dev container made another: %s, was %s", again.ContainerID, first.ContainerID)
	}
	read("on-create\nupdate-content\npost-create\npost-start\npost-attach\npost-start\npost-attach\nbeside\ncreated\ninitialized\ninitialized\n")

	// A rebuild replaces the container and runs every command again; the
	// volume is the one the first container wrote to.
	rebuilt := devcontainerUpIn(t, dir, "--remove-existing-container")
	if rebuilt.ContainerID == first.ContainerID {
		t.Errorf("a rebuild kept the container %s", first.ContainerID)
	}
	if _, err := docker(t, "inspect", first.ContainerID); err == nil {
		t.Errorf("a rebuild left the container it replaced, %s", first.ContainerID)
	}
	read("on-create\nupdate-content\npost-create\npost-start\npost-attach\nbeside\ncreated\ncreated\ninitialized\ninitialized\ninitialized\n")
}

// A dev container whose image has users of its own, as most dev container
// images do: containerUser runs the container, remoteUser the lifecycle
// commands and devcontainer exec, with that user's home and the
// environment remoteEnv builds from the container's and the host's. The
// workspace belongs to the remote user, as updateRemoteUserUID arranges on
// Linux (the CLI applies it on Linux hosts alone; here every process runs
// as the engine's user, who owns the workspace on the host): Git works in
// it, and what the user writes there is the host user's on the host.
func TestDevcontainerUsers(t *testing.T) {
	t.Setenv("FWS_E2E_LOCAL", "local")
	dir, res := devcontainerUp(t, map[string]string{
		".devcontainer/Dockerfile": `FROM fws/base:1
RUN echo 'dev:x:1000:1000:dev:/home/dev:/bin/bash' >> /etc/passwd && echo 'dev:x:1000:' >> /etc/group \
 && echo 'app:x:1001:1001:app:/home/app:/bin/bash' >> /etc/passwd && echo 'app:x:1001:' >> /etc/group \
 && mkdir -p /home/dev /home/app && chown dev:dev /home/dev && chown app:app /home/app
`,
		".devcontainer/devcontainer.json": `{
  "build": {"dockerfile": "Dockerfile"},
  "containerUser": "app",
  "remoteUser": "dev",
  "updateRemoteUserUID": true,
  "containerEnv": {"FROM_CONTAINER": "container"},
  "remoteEnv": {"PATH": "${containerEnv:PATH}:/home/dev/bin", "FROM_LOCAL": "${localEnv:FWS_E2E_LOCAL}"},
  "postCreateCommand": "id -un > /tmp/created-by"
}
`,
	})
	if res.RemoteUser != "dev" {
		t.Errorf("devcontainer up: %+v, want the remote user dev", res)
	}
	if out, err := exec.Command("git", "init", "--quiet", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	out, err := devcontainer(t, "exec", "--workspace-folder", dir, "sh", "-c",
		`id -un; id -u; echo $HOME; cat /tmp/created-by; echo $FROM_CONTAINER $FROM_LOCAL ${PATH##*:}; echo by dev > made-by-dev; stat -c %U made-by-dev; git status --porcelain`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "dev\n1000\n/home/dev\ndev\ncontainer local /home/dev/bin\ndev\n?? .devcontainer/\n?? made-by-dev\n"; out != want {
		t.Errorf("the remote user's command printed %q, want %q", out, want)
	}
	if out := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.Config.User}}", res.ContainerID)); out != "app" {
		t.Errorf("the container runs as %q, want containerUser app", out)
	}
	if out := strings.TrimSpace(mustDocker(t, "exec", res.ContainerID, "id", "-un")); out != "app" {
		t.Errorf("docker exec runs as %q, want the container's user app", out)
	}
	fi, err := os.Stat(filepath.Join(dir, "made-by-dev"))
	if err != nil {
		t.Fatal(err)
	}
	if uid := fi.Sys().(*syscall.Stat_t).Uid; int(uid) != os.Getuid() {
		t.Errorf("the file the remote user wrote in the workspace belongs to uid %d on the host, want %d", uid, os.Getuid())
	}
}

// Docker outside of Docker, as the docker-outside-of-docker feature sets
// it up on Linux: the engine's socket mounted into the dev container, with
// fws/dind's darwin Docker CLI, drives the engine that runs the container
// (the CLI overrides the image's command, so no daemon of its own starts).
// The container finds itself, and runs a sibling that mounts the workspace
// by its host path, which remoteEnv hands in. The Dockerfile builds on
// fws/dind rather than copying its store into another image: a layer of
// the whole store took four minutes to export on a CI runner.
func TestDevcontainerDockerOutsideOfDocker(t *testing.T) {
	sock := strings.TrimPrefix(dockerHost, "unix://")
	dir, res := devcontainerUp(t, map[string]string{
		"README": "from the workspace\n",
		".devcontainer/Dockerfile": `FROM fws/dind:29
`,
		".devcontainer/devcontainer.json": `{
  "build": {"dockerfile": "Dockerfile"},
  "mounts": ["type=bind,source=` + sock + `,target=/var/run/docker.sock"],
  "remoteEnv": {"LOCAL_WORKSPACE_FOLDER": "${localWorkspaceFolder}"}
}
`,
	})
	out, err := devcontainer(t, "exec", "--workspace-folder", dir, "sh", "-c",
		`docker ps -q --no-trunc && docker run --rm -v "$LOCAL_WORKSPACE_FOLDER:/w" fws/base:1 cat /w/README`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, res.ContainerID+"\n") || !strings.HasSuffix(out, "from the workspace\n") {
		t.Errorf("the Docker CLI in the dev container printed %q; want the container %s listed and the sibling's output", out, res.ContainerID)
	}
}

// Playwright in a dev container on the catalog's Playwright image, which
// carries the commands the CLI runs in it: a test in the workspace,
// run by devcontainer exec as a developer runs it, requires the image's
// playwright package, drives its Chromium and saves a screenshot to the
// workspace.
func TestDevcontainerPlaywright(t *testing.T) {
	dir, _ := devcontainerUp(t, map[string]string{
		"page.js": `const { chromium } = require("playwright");
(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage();
  await page.setContent("<title>rendered in a dev container</title><p id=p>painted</p>");
  console.log("title", await page.title(), "text", await page.textContent("#p"));
  await page.screenshot({ path: "shot.png" });
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });
`,
		".devcontainer/devcontainer.json": `{"image": "fws/playwright:1.65"}
`,
	})
	out, err := devcontainer(t, "exec", "--workspace-folder", dir, "node", "page.js")
	if err != nil {
		t.Fatal(err)
	}
	if out != "title rendered in a dev container text painted\n" {
		t.Errorf("Playwright in the dev container printed %q", out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "shot.png")); err != nil || len(b) < 1000 || !strings.HasPrefix(string(b), "\x89PNG") {
		t.Errorf("the screenshot in the workspace: %d bytes, %v", len(b), err)
	}
}

// VS Code's server, the program its Dev Containers extension starts in a
// dev container, runs in one: Microsoft's own darwin build, unpacked into
// the workspace, listens on a port of the container that appPort
// publishes, and answers there with the commit it was built from.
func TestDevcontainerVSCodeServer(t *testing.T) {
	port := fmt.Sprint(freePort(t))

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	server, err := os.MkdirTemp(home, "fws-e2e-vscode-server-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(server) })
	if server, err = filepath.EvalSymlinks(server); err != nil {
		t.Fatal(err)
	}
	// The archive is a zip, whose files' modes tar reads only from a file.
	fetch := exec.Command("sh", "-c", `curl -fsSL --retry 3 -o "$1.zip" https://update.code.visualstudio.com/latest/server-darwin-arm64/stable && tar -xf "$1.zip" --strip-components 1 -C "$1"; s=$?; rm -f "$1.zip"; exit $s`, "sh", server)
	if out, err := fetch.CombinedOutput(); err != nil {
		t.Fatalf("fetching VS Code's server: %v\n%s", err, out)
	}

	_, up := devcontainerUp(t, map[string]string{
		".devcontainer/devcontainer.json": `{
  "image": "fws/base:1",
  "mounts": ["type=bind,source=` + server + `,target=/vscode-server"],
  "appPort": ["127.0.0.1:` + port + `:8000"],
  "postStartCommand": "nohup /vscode-server/bin/code-server --host 0.0.0.0 --port 8000 --without-connection-token --accept-server-license-terms > /tmp/server.log 2>&1 &"
}
`,
	})
	var version string
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		res, err := pollClient.Get("http://127.0.0.1:" + port + "/version")
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		version = strings.TrimSpace(string(b))
		break
	}
	if len(version) != 40 {
		log, _ := docker(t, "exec", up.ContainerID, "/bin/sh", "-c", "exec 2>&1; cat /tmp/server.log; ls -l /vscode-server/node; ps -ef; /vscode-server/node --version; echo node: $?; /vscode-server/bin/code-server --version; echo code-server: $?; cat /vscode-server/bin/code-server")
		t.Fatalf("VS Code's server answered %q on the published port; its log:\n%s\nSeatbelt refused:\n%s", version, log, sandboxDenials(t, "node"))
	}
}

// What an exec leaves running in the background outlives it, as in Docker,
// where a dev container's lifecycle commands start servers so, and ends
// with the container.
func TestExecLeavesItsBackgroundProcesses(t *testing.T) {
	const name = "fws-e2e-exec-background"
	mustDocker(t, "run", "-d", "--name", name, "fws/base:1", "sleep", "600")
	defer docker(t, "rm", "-f", name)
	mustDocker(t, "exec", name, "sh", "-c", "(sleep 4; echo late > /tmp/late) > /dev/null 2>&1 &")
	var out string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if out, _ = docker(t, "exec", name, "cat", "/tmp/late"); out != "" {
			break
		}
	}
	if out != "late\n" {
		t.Errorf("the background process an exec left wrote %q", out)
	}
}
