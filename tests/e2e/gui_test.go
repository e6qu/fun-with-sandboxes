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

// A graphical application runs in a container granted the user's session
// with --security-opt seatbelt=gui: Electron, as its project ships it for
// macOS, opens a window, renders a page in it, captures what it drew and
// writes to the pasteboard, where the host reads it.
func TestElectronInAContainer(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, "fws-e2e-electron-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	zip := filepath.Join(dir, "electron.zip")
	url := "https://github.com/electron/electron/releases/download/v" + electronVersion + "/electron-v" + electronVersion + "-darwin-arm64.zip"
	if out, err := exec.Command("curl", "-fsSL", "--retry", "3", "-o", zip, url).CombinedOutput(); err != nil {
		t.Fatalf("fetching Electron: %v\n%s", err, out)
	}
	// ditto keeps the frameworks' symbolic links.
	if out, err := exec.Command("ditto", "-xk", zip, dir).CombinedOutput(); err != nil {
		t.Fatalf("unpacking Electron: %v\n%s", err, out)
	}
	os.Remove(zip)
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
