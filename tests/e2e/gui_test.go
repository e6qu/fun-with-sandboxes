//go:build e2e && darwin

package e2e

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// electronVersion is the Electron release the test runs, Electron's own
// darwin build.
const electronVersion = "38.2.0"

// crashReport is the start of the newest crash report macOS wrote in the
// last minutes for a process named like name: what killed it and where.
func crashReport(name string) string {
	home, _ := os.UserHomeDir()
	var newest string
	var at time.Time
	for _, dir := range []string{filepath.Join(home, "Library/Logs/DiagnosticReports"), "/Library/Logs/DiagnosticReports"} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			fi, err := e.Info()
			if err != nil || !strings.Contains(strings.ToLower(e.Name()), name) || time.Since(fi.ModTime()) > 10*time.Minute {
				continue
			}
			if fi.ModTime().After(at) {
				newest, at = filepath.Join(dir, e.Name()), fi.ModTime()
			}
		}
	}
	if newest == "" {
		return "none"
	}
	b, err := os.ReadFile(newest)
	if err != nil {
		return err.Error()
	}
	if len(b) > 16<<10 {
		b = b[:16<<10]
	}
	return newest + "\n" + string(b)
}

// sandboxDenials lists what Seatbelt refused the processes named like
// name in the last minutes, each operation once: what a profile lacks.
func sandboxDenials(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("/usr/bin/log", "show", "--last", "5m", "--style", "compact",
		"--predicate", `sender == "Sandbox" AND eventMessage CONTAINS "deny" AND eventMessage CONTAINS "`+name+`"`).CombinedOutput()
	if err != nil {
		return "log show: " + err.Error() + "\n" + string(out)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "deny("); i >= 0 {
			// Past "deny(1) ": the operation and what it named.
			if _, rest, ok := strings.Cut(line[i:], " "); ok {
				seen[rest] = true
			}
		}
	}
	var lines []string
	for l := range seen {
		lines = append(lines, l)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// unzipped fetches the zip at url and unpacks it into a new directory under
// the user's home, which a container then mounts, and returns the
// directory; it is removed when the test ends.
func unzipped(t *testing.T, prefix, url string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	zip := filepath.Join(dir, "download.zip")
	if out, err := exec.Command("curl", "-fsSL", "--retry", "3", "-o", zip, url).CombinedOutput(); err != nil {
		t.Fatalf("fetching %s: %v\n%s", url, err, out)
	}
	// ditto keeps a bundle's symbolic links and modes.
	if out, err := exec.Command("ditto", "-xk", zip, dir).CombinedOutput(); err != nil {
		t.Fatalf("unpacking %s: %v\n%s", url, err, out)
	}
	os.Remove(zip)
	return dir
}

// A graphical application runs in a container granted the user's session
// with --security-opt seatbelt=gui: Electron, as its project ships it for
// macOS, opens a window, renders a page in it, captures what it drew and
// writes to the pasteboard, where the host reads it.
func TestElectronInAContainer(t *testing.T) {
	dir := unzipped(t, "fws-e2e-electron-", "https://github.com/electron/electron/releases/download/v"+electronVersion+"/electron-v"+electronVersion+"-darwin-arm64.zip")
	if err := os.WriteFile(filepath.Join(dir, "main.js"), []byte(`const { app, BrowserWindow, clipboard } = require("electron");
setTimeout(() => { console.log("timed out"); process.exit(3); }, 90000);
app.whenReady().then(async () => {
  const win = new BrowserWindow({ width: 400, height: 300, show: true });
  await win.loadURL("data:text/html,<title>drawn in fws</title><body style='background:red'><p id=p>painted</p>");
  const text = await win.webContents.executeJavaScript("document.getElementById('p').textContent");
  console.log("title", win.getTitle(), "text", text, "visible", win.isVisible());
  const size = (await win.webContents.capturePage()).getSize();
  console.log("captured", size.width > 0 && size.height > 0);
  clipboard.writeText("copied in a container");
  console.log("pasteboard", clipboard.readText());
  app.quit();
});
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Chromium's helpers cannot enter a sandbox of their own from inside
	// the container's, which confines them as it does the application. The
	// application runs as the engine's user: the session's services tell
	// who calls them from the kernel, and refuse a caller that names
	// itself root, as a container's default user does.
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	out, err := docker(t, "run", "--rm", "--security-opt", "seatbelt=gui", "--user", user, "-v", dir+":/app",
		"fws/base:1", "/app/Electron.app/Contents/MacOS/Electron", "--no-sandbox", "/app/main.js")
	for _, want := range []string{"title drawn in fws text painted visible true", "captured true", "pasteboard copied in a container"} {
		if !strings.Contains(out, want) {
			t.Errorf("Electron printed %q, %v; want %q", out, err, want)
		}
	}
	if t.Failed() {
		t.Logf("Seatbelt refused Electron:\n%s", sandboxDenials(t, "Electron"))
		// Again, with the library telling what Electron asked about files'
		// places: the last of it is where it lost its way.
		_, terr := docker(t, "run", "--rm", "--security-opt", "seatbelt=gui", "--user", user, "-e", "FWS_TRACE=1", "-v", dir+":/app",
			"fws/base:1", "/app/Electron.app/Contents/MacOS/Electron", "--no-sandbox", "/app/main.js")
		var calls []string
		for _, line := range strings.Split(fmt.Sprint(terr), "\n") {
			if strings.HasPrefix(line, "fws: ") && !strings.Contains(line, "/Electron Framework.framework/") {
				calls = append(calls, line)
			}
		}
		t.Logf("%d calls traced; the first:\n%s", len(calls), strings.Join(calls[:min(len(calls), 60)], "\n"))
		t.Logf("the last:\n%s", strings.Join(calls[max(0, len(calls)-260):], "\n"))
		return
	}
	if b, err := exec.Command("pbpaste").Output(); err != nil || string(b) != "copied in a container" {
		t.Errorf("the host's pasteboard holds %q, %v", b, err)
	}
}

// VS Code itself, Microsoft's macOS build, runs in a container granted the
// user's session: it opens its window on a workspace, loads an extension,
// whose test edits and saves a file through the editor and runs a command
// in the integrated terminal, and exits with the test's result.
func TestVSCodeInAContainer(t *testing.T) {
	dir := unzipped(t, "fws-e2e-vscode-", "https://update.code.visualstudio.com/latest/darwin-arm64/stable")
	const app = "Visual Studio Code.app"
	name, err := exec.Command("/usr/bin/defaults", "read", filepath.Join(dir, app, "Contents", "Info"), "CFBundleExecutable").Output()
	if err != nil {
		t.Fatalf("reading the application's executable name: %v", err)
	}
	for file, body := range map[string]string{
		"work/note.txt":    "first\n",
		"ext/package.json": `{"name": "fws-e2e", "publisher": "fws", "version": "0.0.1", "engines": {"vscode": "*"}, "main": "./extension.js", "activationEvents": ["*"]}`,
		"ext/extension.js": "exports.activate = () => {};\n",
		"ext/test.js": `const vscode = require("vscode");
const fs = require("fs");
exports.run = async () => {
  const doc = await vscode.workspace.openTextDocument("/work/note.txt");
  const editor = await vscode.window.showTextDocument(doc);
  await editor.edit((e) => e.insert(new vscode.Position(1, 0), "edited in the editor\n"));
  if (!(await doc.save())) throw new Error("the document did not save");
  const terminal = vscode.window.createTerminal({ name: "fws", shellPath: "/bin/bash" });
  terminal.sendText("echo typed in the terminal > /work/terminal.txt; uname -n >> /work/terminal.txt");
  for (let i = 0; i < 300 && !(fs.existsSync("/work/terminal.txt") && fs.readFileSync("/work/terminal.txt", "utf8").split("\n").length > 2); i++) {
    await new Promise((r) => setTimeout(r, 200));
  }
  fs.writeFileSync("/work/result.txt", "window " + vscode.window.state.focused + " folders " + vscode.workspace.workspaceFolders.map((f) => f.uri.fsPath).join(","));
};
`,
	} {
		p := filepath.Join(dir, file)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The application runs as the engine's user, as the session's services
	// ask, without Chromium's own sandbox and without the keychain, which
	// the grant withholds.
	out, err := docker(t, "run", "--rm", "--security-opt", "seatbelt=gui", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--hostname", "devbox", "-e", "SHELL=/bin/bash", "-e", "HOME=/tmp/home",
		"-v", dir+":/vscode", "-v", filepath.Join(dir, "work")+":/work", "-v", filepath.Join(dir, "ext")+":/ext", "fws/base:1",
		"/vscode/"+app+"/Contents/MacOS/"+strings.TrimSpace(string(name)),
		"--no-sandbox", "--use-mock-keychain", "--user-data-dir=/tmp/vscode-data", "--extensions-dir=/tmp/vscode-extensions",
		"--disable-workspace-trust", "--skip-welcome", "--skip-release-notes", "--disable-updates", "--disable-telemetry",
		"--extensionDevelopmentPath=/ext", "--extensionTestsPath=/ext/test.js", "/work")
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, "work", name))
		return string(b)
	}
	if err != nil {
		t.Errorf("VS Code: %v\n%s", err, out)
	}
	if got := read("note.txt"); got != "first\nedited in the editor\n" {
		t.Errorf("the file the editor saved holds %q", got)
	}
	if got := read("terminal.txt"); got != "typed in the terminal\ndevbox\n" {
		t.Errorf("the integrated terminal's command wrote %q", got)
	}
	if got := read("result.txt"); !strings.Contains(got, "folders /work") {
		t.Errorf("the extension's test reported %q", got)
	}
	if t.Failed() {
		t.Logf("Seatbelt refused VS Code:\n%s", sandboxDenials(t, "Code"))
	}
}

// devContainerWindow is a VS Code window on the host that Microsoft's Dev
// Containers extension has connected to a dev container of the engine's.
type devContainerWindow struct {
	// data is VS Code's user data directory, which names its processes.
	data string
	// id is the dev container's.
	id     string
	window *exec.Cmd
	exited chan struct{}
}

// openInDevContainer opens a project folder made of files in a dev
// container from VS Code on the host, with env added to VS Code's
// environment: the extension creates the container through the Docker CLI,
// installs VS Code's server in it and connects the window to it. It
// returns once the server serves the window, and fails the test with
// every log that tells why if it does not. The project is at
// /workspaces/project in the container.
func openInDevContainer(t *testing.T, files map[string]string, env ...string) *devContainerWindow {
	t.Helper()
	dir := unzipped(t, "fws-e2e-vscode-host-", "https://update.code.visualstudio.com/latest/darwin-arm64/stable")
	const app = "Visual Studio Code.app"
	cli := filepath.Join(dir, app, "Contents", "Resources", "app", "bin", "code")
	data, exts, work := filepath.Join(dir, "data"), filepath.Join(dir, "extensions"), filepath.Join(dir, "project")
	for name, body := range files {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The extension says each step it takes, and each command it runs in
	// the container, at this level of its log.
	if err := os.MkdirAll(filepath.Join(data, "User"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "User", "settings.json"), []byte(`{"dev.containers.logLevel": "trace"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := func(args ...string) *exec.Cmd {
		cmd := exec.Command(cli, append([]string{"--user-data-dir", data, "--extensions-dir", exts}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		return cmd
	}
	if out, err := code("--install-extension", "ms-vscode-remote.remote-containers").CombinedOutput(); err != nil {
		t.Fatalf("installing the Dev Containers extension: %v\n%s", err, out)
	}

	// The extension installs VS Code's server in the volume it shares
	// between dev containers, of the build the container's /etc/os-release
	// names: the engine's, ID=darwin, has it install the macOS server. The
	// volume starts empty, so the server that comes up is the one the
	// extension installed; no Linux build could run in the container.
	docker(t, "volume", "rm", "-f", "vscode")

	uri := "vscode-remote://dev-container+" + hex.EncodeToString([]byte(work)) + "/workspaces/project"
	window := code("--new-window", "--disable-workspace-trust", "--skip-welcome", "--skip-release-notes", "--disable-updates",
		"--disable-telemetry", "--use-mock-keychain", "--wait", "--folder-uri", uri)
	var output lockedBuffer
	window.Stdout, window.Stderr = &output, &output
	if err := window.Start(); err != nil {
		t.Fatal(err)
	}
	w := &devContainerWindow{data: data, window: window, exited: make(chan struct{})}
	go func() { window.Wait(); close(w.exited) }()
	t.Cleanup(func() {
		exec.Command("pkill", "-f", data).Run()
		select {
		case <-w.exited:
		case <-time.After(time.Minute):
			window.Process.Kill()
		}
		if ids, _ := docker(t, "ps", "-aq", "--filter", "label=devcontainer.local_folder="+work); strings.TrimSpace(ids) != "" {
			docker(t, append([]string{"rm", "-f", "-v"}, strings.Fields(ids)...)...)
		}
		docker(t, "volume", "rm", "-f", "vscode")
	})

	// The extension has made the container, and the window works in it,
	// when VS Code's server there listens and has launched the window's
	// extension host, which it does once the window has connected through
	// the extension's forwarded port. The extension copies the server's
	// output into its own log, on the host.
	var serverLog string
	up := func() bool {
		return strings.Contains(serverLog, "Extension host agent listening") && strings.Contains(serverLog, "Launched Extension Host Process")
	}
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if w.id == "" {
			ids, _ := docker(t, "ps", "-q", "--filter", "label=devcontainer.local_folder="+work)
			w.id = strings.TrimSpace(ids)
			continue
		}
		serverLog = extensionLog(data)
		if up() {
			break
		}
	}
	if w.id == "" || !up() {
		var listing string
		if w.id != "" {
			top, _ := docker(t, "top", w.id)
			listing, _ = docker(t, "exec", w.id, "/bin/sh", "-c", "exec 2>&1; ls -la /root/.vscode-server /tmp /tmp/devcontainers-* | head -80")
			listing = top + "\n" + listing + "\nThe helper's channel, replayed with the container's node:\n" + helperChannel(t, w.id) + "\nThe extension's helper, run by hand:\n" + helperRun(t, w.id)
		}
		t.Fatalf("the Dev Containers extension did not bring VS Code's server up in a container (container %q).\nIn the container:\n%s\nVS Code printed:\n%s\nThe extension's logs:\n%s\nThe extension's code, where it logs its last steps:\n%s",
			w.id, listing, output.String(), windowLogs(data), extensionCode(exts))
	}
	return w
}

// windowLogs is every log of the windows of the user data directory data,
// the last of each: the extension's own, whole, and the window's, where
// what the extension waits on shows.
func windowLogs(data string) string {
	var logs strings.Builder
	filepath.Walk(filepath.Join(data, "logs"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
			return nil
		}
		keep := int64(4000)
		switch {
		case strings.Contains(p, "remoteContainers-"):
			// The extension's trace, whole: each step it took.
			keep = 1 << 20
		case strings.Contains(p, "renderer"), strings.Contains(p, "exthost.log"):
			keep = 12000
		}
		b, _ := os.ReadFile(p)
		if int64(len(b)) > keep {
			b = b[int64(len(b))-keep:]
		}
		fmt.Fprintf(&logs, "== %s (%d bytes)\n%s\n", strings.TrimPrefix(p, data), fi.Size(), b)
		return nil
	})
	return logs.String()
}

// inContainer is what the dev container's files, VS Code server's logs and
// processes show, for a test that failed to find what it looked for there.
func (w *devContainerWindow) inContainer(t *testing.T) string {
	t.Helper()
	out, _ := docker(t, "exec", w.id, "/bin/sh", "-c", "exec 2>&1; ls -la /tmp /root /root/.vscode-server /root/.vscode-server/extensions; for f in /tmp/attached.trace /tmp/serve.log; do echo \"== $f\"; cat \"$f\"; done; cat /root/.gitconfig /etc/gitconfig; for f in $(find /root/.vscode-server/data/logs -name '*.log' | head -20); do echo \"== $f\"; tail -n 40 \"$f\"; done")
	top, _ := docker(t, "top", w.id)
	return top + "\n" + out + "\nThe extension's log:\n" + extensionLog(w.data)
}

// quit quits VS Code as a user does: its main process takes SIGTERM as a
// request to quit, which closes its windows and deactivates the
// extensions, and the window's launcher then returns.
func (w *devContainerWindow) quit(t *testing.T) {
	t.Helper()
	pids, _ := exec.Command("pgrep", "-f", w.data).Output()
	for _, pid := range strings.Fields(string(pids)) {
		cmdline, _ := exec.Command("ps", "-o", "command=", "-p", pid).Output()
		// Not the helpers, whose own executables live in the bundle's
		// frameworks, nor the launcher, which runs the CLI script.
		if c := string(cmdline); strings.Contains(c, ".app/Contents/MacOS/") && !strings.Contains(c, "Helper") && !strings.Contains(c, "cli.js") {
			exec.Command("kill", "-TERM", pid).Run()
		}
	}
	select {
	case <-w.exited:
	case <-time.After(time.Minute):
		t.Errorf("VS Code did not quit within a minute of SIGTERM")
	}
}

// VS Code on the host, with Microsoft's Dev Containers extension, opens a
// folder in a dev container of the engine's: the extension creates the
// container through the Docker CLI, installs VS Code's server in it and
// connects the window to it.
func TestVSCodeDevContainersExtension(t *testing.T) {
	w := openInDevContainer(t, map[string]string{
		".devcontainer/devcontainer.json": `{"name": "fws e2e", "image": "fws/base:1"}` + "\n",
		"README":                          "from the workspace\n",
	})
	if out, err := docker(t, "exec", w.id, "cat", "/workspaces/project/README"); err != nil || out != "from the workspace\n" {
		t.Errorf("the workspace in the container: %q, %v", out, err)
	}
}

// What a devcontainer.json asks of VS Code itself, beyond what the Dev
// Containers CLI does: the extensions customizations.vscode.extensions
// names are installed in the container's VS Code server, from the
// Marketplace, and its settings become the server's machine settings;
// postAttachCommand runs once the window attaches; forwardPorts forwards a
// port of the container that nothing publishes to the same port on the
// host, as portsAttributes' requireLocalPort demands; and quitting VS Code
// stops the container, the default shutdownAction.
func TestVSCodeDevContainersCustomizations(t *testing.T) {
	port := fmt.Sprint(freePort(t))
	w := openInDevContainer(t, map[string]string{
		".devcontainer/devcontainer.json": `{
  "image": "fws/base:1",
  "customizations": {"vscode": {
    "extensions": ["editorconfig.editorconfig"],
    "settings": {"fws.e2e.setting": "from devcontainer.json"}
  }},
  "forwardPorts": [` + port + `],
  "portsAttributes": {"` + port + `": {"label": "fws e2e", "onAutoForward": "silent", "requireLocalPort": true}},
  "postAttachCommand": "sh .devcontainer/attached.sh ` + port + `"
}
`,
		// A server on the port, run by the node VS Code's server brings,
		// as fws/base has none of its own.
		// Each step is traced, for a failure to show how far it came.
		".devcontainer/attached.sh": `exec 2> /tmp/attached.trace
set -x
echo attached > /tmp/attached
node=$(ls /root/.vscode-server/bin/*/node | head -1)
nohup "$node" .devcontainer/serve.js "$1" > /tmp/serve.log 2>&1 &
echo "started $!" >&2
`,
		".devcontainer/serve.js": `require("http").createServer((req, res) => res.end("forwarded\n")).listen(Number(process.argv[2]));
`,
	})

	var installed, settings, attached, forwarded string
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		installed, _ = docker(t, "exec", w.id, "ls", "/root/.vscode-server/extensions")
		settings, _ = docker(t, "exec", w.id, "cat", "/root/.vscode-server/data/Machine/settings.json")
		attached, _ = docker(t, "exec", w.id, "cat", "/tmp/attached")
		if res, err := pollClient.Get("http://127.0.0.1:" + port + "/"); err == nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			forwarded = string(b)
		}
		if strings.Contains(installed, "editorconfig.editorconfig-") && strings.Contains(settings, "from devcontainer.json") && attached == "attached\n" && forwarded == "forwarded\n" {
			break
		}
	}
	if !strings.Contains(installed, "editorconfig.editorconfig-") {
		t.Errorf("the server's extensions: %q, want EditorConfig's", installed)
	}
	if !strings.Contains(settings, "from devcontainer.json") {
		t.Errorf("the server's machine settings: %q", settings)
	}
	if attached != "attached\n" {
		t.Errorf("postAttachCommand left %q", attached)
	}
	if forwarded != "forwarded\n" {
		t.Errorf("the forwarded port %s on the host answered %q", port, forwarded)
	}
	if t.Failed() {
		t.Fatalf("in the container:\n%s", w.inContainer(t))
	}

	w.quit(t)
	var running string
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if running = strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.Running}}", w.id)); running == "false" {
			break
		}
	}
	if running != "false" {
		t.Errorf("the dev container still runs a minute after VS Code quit; the extension's log:\n%s", extensionLog(w.data))
	}
}

// The Dev Containers extension lends the container the host user's Git
// and SSH: it copies the host's Git configuration into the container,
// answers Git's credential requests there with the host's credential
// helper, and forwards the host's SSH agent to a socket in the container.
// The host's helper reads a file the container cannot, so only the
// extension's forwarding answers with it; the agent holds a key made for
// the test.
func TestVSCodeDevContainersForwardsCredentials(t *testing.T) {
	// The host user's home for VS Code: its .gitconfig, which the
	// extension copies, and whose helper the host's Git runs.
	home, err := os.MkdirTemp(os.Getenv("HOME"), "fws-e2e-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err := os.WriteFile(filepath.Join(home, "credential"), []byte("username=fws-e2e\npassword=from-the-host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = fws e2e\n\temail = e2e@fws.invalid\n[credential]\n\thelper = \"!f() { cat '"+filepath.Join(home, "credential")+"'; }; f\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// An SSH agent holding a key of its own. Its socket's path is short,
	// as a Unix socket's must be.
	agentDir, err := os.MkdirTemp("/tmp", "fws-e2e-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(agentDir) })
	sock, key := filepath.Join(agentDir, "agent.sock"), filepath.Join(agentDir, "key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "fws-e2e-agent", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	agent := exec.Command("ssh-agent", "-D", "-a", sock)
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Process.Kill(); agent.Wait() })
	add := exec.Command("ssh-add", key)
	add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	var out []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(sock); err == nil {
			out, err = add.CombinedOutput()
			if err != nil {
				t.Fatalf("ssh-add: %v\n%s", err, out)
			}
			break
		}
	}
	if out == nil {
		t.Fatalf("ssh-agent made no socket at %s", sock)
	}

	w := openInDevContainer(t, map[string]string{
		".devcontainer/devcontainer.json": `{"image": "fws/base:1"}` + "\n",
		// Lists the agent's identities: SSH2_AGENTC_REQUEST_IDENTITIES,
		// answered by SSH2_AGENT_IDENTITIES_ANSWER (12), its count and the
		// keys with their comments.
		"agent.js": `const c = require("net").connect(process.env.SSH_AUTH_SOCK);
let b = Buffer.alloc(0);
c.on("connect", () => c.write(Buffer.from([0, 0, 0, 1, 11])));
c.on("data", (d) => {
  b = Buffer.concat([b, d]);
  if (b.length >= 4 && b.length >= 4 + b.readUInt32BE(0)) {
    console.log("answer", b[4], "keys", b.readUInt32BE(5), "comment", b.includes("fws-e2e-agent"));
    c.end();
  }
});
c.on("error", (e) => { console.log("error", e.message); process.exit(1); });
`,
	}, "HOME="+home, "SSH_AUTH_SOCK="+sock)

	// The extension's helper in the container, which the credential helper
	// it configures and the agent's socket talk to, is reached through a
	// socket its terminals name in REMOTE_CONTAINERS_IPC; a docker exec
	// finds it by its name.
	var name, credential, identities string
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		name, _ = docker(t, "exec", w.id, "git", "config", "--global", "user.name")
		credential, _ = docker(t, "exec", w.id, "/bin/sh", "-c",
			`REMOTE_CONTAINERS_IPC=$(ls /tmp/vscode-remote-containers-ipc-*.sock | head -1) && export REMOTE_CONTAINERS_IPC && printf 'protocol=https\nhost=example.com\n\n' | git credential fill`)
		identities, _ = docker(t, "exec", w.id, "/bin/sh", "-c",
			`SSH_AUTH_SOCK=$(ls /tmp/vscode-ssh-auth-*.sock | head -1) && export SSH_AUTH_SOCK && exec "$(ls /root/.vscode-server/bin/*/node | head -1)" /workspaces/project/agent.js`)
		if name == "fws e2e\n" && strings.Contains(credential, "password=from-the-host") && identities == "answer 12 keys 1 comment true\n" {
			return
		}
	}
	t.Errorf("the host's Git configuration in the container: user.name %q", name)
	t.Errorf("git credential fill in the container: %q, want the host helper's password", credential)
	t.Errorf("the forwarded SSH agent: %q, want the host agent's one key", identities)
	t.Fatalf("in the container:\n%s", w.inContainer(t))
}

// extensionLog is the Dev Containers extension's log in the window's logs
// under the user data directory data: the last one, by its name's time.
func extensionLog(data string) string {
	var last string
	filepath.Walk(filepath.Join(data, "logs"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && strings.HasPrefix(fi.Name(), "remoteContainers-") && fi.Name() > filepath.Base(last) {
			last = p
		}
		return nil
	})
	if last == "" {
		return ""
	}
	b, _ := os.ReadFile(last)
	return string(b)
}

// extensionCode is the Dev Containers extension's own code around the
// messages it logs as it launches its helper and probes the container's
// environment, the steps its log ends with: what it does next, which the
// log does not say.
func extensionCode(exts string) string {
	files, _ := filepath.Glob(filepath.Join(exts, "ms-vscode-remote.remote-containers-*", "dist", "*", "*.js"))
	var out strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, mark := range []string{"await rk(", ",rk(", "(rk(", "await MH(", "Not reconnecting"} {
			for i, from := 0, 0; i < 6; i++ {
				at := strings.Index(string(b[from:]), mark)
				if at < 0 {
					break
				}
				at += from
				lo, hi := max(at-1500, 0), min(at+3000, len(b))
				fmt.Fprintf(&out, "== %s at %d (%q):\n%s\n\n", filepath.Base(f), at, mark, b[lo:hi])
				from = at + len(mark)
			}
		}
	}
	return out.String()
}

// helperChannel replays how the Dev Containers extension talks to its
// helper: a shell in docker exec -i that it writes a command line to, which
// marks its start on stderr and runs node, whose stdin and stdout, the
// shell's own, then carry the extension's RPC. It returns what came back.
func helperChannel(t *testing.T, id string) string {
	t.Helper()
	node, err := docker(t, "exec", id, "/bin/sh", "-c", "ls /root/.vscode-server/bin/*/node | head -1")
	if err != nil {
		return "no node: " + err.Error()
	}
	cmd := exec.Command("docker", "exec", "-i", id, "/bin/sh")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err.Error()
	}
	var out, errOut lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Start(); err != nil {
		return err.Error()
	}
	fmt.Fprintf(stdin, "set -e ; echo -n MARK >&2 ; '%s' -e 'process.stdout.write(\"started\\n\"); process.stdin.on(\"data\", d => process.stdout.write(\"echo:\" + d))' ; exit\n", strings.TrimSpace(node))
	time.Sleep(5 * time.Second)
	io.WriteString(stdin, "ping\n")
	time.Sleep(5 * time.Second)
	stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		err = fmt.Errorf("still running 10s after its stdin closed")
	}
	return fmt.Sprintf("stdout %q, stderr %q (exit: %v)", out.String(), errOut.String(), err)
}

// helperRun runs the extension's own helper script in the container as the
// extension does, with an IPC socket of its own, its stdin held open for a
// few seconds, and returns what it wrote, where a failure as it starts
// shows; and the script's code around what it reads of the system.
func helperRun(t *testing.T, id string) string {
	t.Helper()
	script, err := docker(t, "exec", id, "/bin/sh", "-c", "ls /tmp/vscode-remote-containers-server-*.js | head -1")
	if err != nil || strings.TrimSpace(script) == "" {
		return "no helper script: " + fmt.Sprint(err)
	}
	script = strings.TrimSpace(script)
	cmd := exec.Command("docker", "exec", "-i", id, "/bin/sh", "-c",
		"REMOTE_CONTAINERS_SOCKETS='[]' REMOTE_CONTAINERS_IPC=/tmp/fws-e2e-helper.sock /root/.vscode-server/bin/*/node '"+script+"'")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err.Error()
	}
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err.Error()
	}
	time.Sleep(8 * time.Second)
	stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		err = fmt.Errorf("still running 10s after its stdin closed")
	}
	code, _ := docker(t, "exec", id, "cat", script)
	var marks strings.Builder
	for _, mark := range []string{"/proc", "ready", "process.stdin", "execSync", "spawn("} {
		for i, from := 0, 0; i < 4; i++ {
			at := strings.Index(code[from:], mark)
			if at < 0 {
				break
			}
			at += from
			fmt.Fprintf(&marks, "-- %q at %d: %s\n", mark, at, code[max(at-300, 0):min(at+500, len(code))])
			from = at + len(mark)
		}
	}
	return fmt.Sprintf("%q (exit: %v)\nIts code (%d bytes):\n%s", out.String(), err, len(code), marks.String())
}
