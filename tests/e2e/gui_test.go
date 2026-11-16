//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// electronVersion is the Electron release the test runs, Electron's own
// darwin build.
const electronVersion = "38.2.0"

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
