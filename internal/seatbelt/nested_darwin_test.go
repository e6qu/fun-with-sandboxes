package seatbelt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// macOS never stacks Seatbelt profiles: a process under any profile, even
// one that allows everything, is refused a second one, through sandbox_init
// and through sandbox_compile_string and sandbox_apply alike, as Chromium's
// helpers enter theirs. So no program in a container keeps a sandbox of its
// own, and Chromium-based applications take --no-sandbox (README). An
// unsandboxed process enters the same profiles, and they hold.
func TestSecondSandboxRefused(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "nested")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-Werror", "-o", probe, "testdata/nested/main.c", "-lsandbox").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	// The file the inner profile denies is in the container's root, which
	// the container's profile lets it read.
	root := filepath.Join(dir, "root")
	os.MkdirAll(root, 0o755)
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Root: root, StateRoot: filepath.Join(dir, "state"), IPCPrefix: IPCPrefixFor(root)}
	container, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.GUI = true
	gui, err := Render(spec)
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
	inners := []string{
		write("inner-allow", "(version 1)\n(allow default)\n(deny file-read-data (literal \""+target+"\"))\n"),
		write("inner-deny", "(version 1)\n(deny default)\n(import \"system.sb\")\n(allow file-read*)\n(deny file-read-data (literal \""+target+"\"))\n"),
	}
	for _, o := range []struct {
		name, path string
		refused    bool
	}{
		{"none", "/dev/null", false},
		{"allow-all", write("allow-all", "(version 1)\n(allow default)\n"), true},
		{"deny-default", write("deny-default", "(version 1)\n(deny default)\n(import \"system.sb\")\n(allow file-read*)\n(allow file-write-data (literal \"/dev/null\"))\n(allow process-fork)\n"), true},
		{"container", write("container", container), true},
		{"container-gui", write("container-gui", gui), true},
	} {
		for _, inner := range inners {
			for _, mode := range []string{"init", "apply"} {
				b, err := exec.Command(probe, o.path, inner, mode, target).CombinedOutput()
				out := string(b)
				if err != nil {
					t.Fatalf("outer %s, %s: %v\n%s", o.name, mode, err, out)
				}
				if !strings.Contains(out, "outer: sandboxed "+map[bool]string{false: "0", true: "1"}[o.refused]+", "+target+" readable 1") {
					t.Fatalf("outer %s, %s: the outer profile did not leave the target readable:\n%s", o.name, mode, out)
				}
				refused := strings.Contains(out, "errno 1 (Operation not permitted)")
				holds := strings.Contains(out, "after: "+target+" readable 0")
				if refused != o.refused || holds == o.refused {
					t.Errorf("outer %s, inner %s, %s: refused %v, holds %v; want refused %v:\n%s", o.name, filepath.Base(inner), mode, refused, holds, o.refused, out)
				}
			}
		}
	}
}
