package seatbelt

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// An experiment, not a check: whether a process under an outer Seatbelt
// profile can enter one of its own, as Chromium's helpers do with
// sandbox_compile_string and sandbox_apply, under outer profiles from
// allowing everything to a container's own, and whether the inner one
// then holds. It prints what macOS answered each time, and the functions
// libsandbox exports, for a way the engine could let Chromium keep its
// sandbox inside a container's.
func TestNestedSandboxExperiment(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "nested")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-Werror", "-o", probe, "testdata/nested/main.c", "-lsandbox").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	os.MkdirAll(root, 0o755)
	container, err := Render(Spec{Root: root, StateRoot: filepath.Join(dir, "state")})
	if err != nil {
		t.Fatal(err)
	}
	gui, err := Render(Spec{Root: root, StateRoot: filepath.Join(dir, "state"), GUI: true})
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) string {
		p := filepath.Join(dir, name+".sb")
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	outers := []struct{ name, path string }{
		{"none", "/dev/null"},
		{"allow-all", write("allow-all", "(version 1)\n(allow default)\n")},
		{"allow-but-one-deny", write("allow-but-one-deny", "(version 1)\n(allow default)\n(deny file-write* (subpath \"/var/empty/fws-nothing\"))\n")},
		{"deny-default", write("deny-default", "(version 1)\n(deny default)\n(import \"system.sb\")\n(allow file-read*)\n(allow file-write-data (literal \"/dev/null\"))\n(allow process-fork)\n")},
		{"container", write("container", container)},
		{"container-gui", write("container-gui", gui)},
	}
	inners := []struct{ name, path string }{
		{"allow-but-target", write("inner-allow", "(version 1)\n(allow default)\n(deny file-read-data (literal \""+target+"\"))\n")},
		{"deny-default-but-reads", write("inner-deny", "(version 1)\n(deny default)\n(import \"system.sb\")\n(allow file-read*)\n(deny file-read-data (literal \""+target+"\"))\n")},
	}
	for _, o := range outers {
		for _, in := range inners {
			for _, mode := range []string{"init", "apply"} {
				out, err := exec.Command(probe, o.path, in.path, mode, target).CombinedOutput()
				t.Logf("nested sandbox experiment, outer %s, inner %s, %s (%v):\n%s", o.name, in.name, mode, err, out)
			}
		}
	}
	// libsandbox's exported functions, from the SDK's stub.
	if sdk, err := exec.Command("/usr/bin/xcrun", "--show-sdk-path").Output(); err == nil {
		b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(sdk)), "usr/lib/libsandbox.1.tbd"))
		if err != nil {
			b, err = os.ReadFile(filepath.Join(strings.TrimSpace(string(sdk)), "usr/lib/libsandbox.tbd"))
		}
		seen := map[string]bool{}
		for _, m := range regexp.MustCompile(`_?(sandbox_[A-Za-z0-9_]+)`).FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
		var names []string
		for n := range seen {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Logf("nested sandbox experiment, libsandbox exports (%v): %s", err, strings.Join(names, " "))
	}
}
