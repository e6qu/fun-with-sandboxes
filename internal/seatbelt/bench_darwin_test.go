package seatbelt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// BenchmarkCompile measures libsandbox's compile of a container's profile,
// which every process the engine starts in a container pays as it enters
// the sandbox, with and without the system profile it imports.
func BenchmarkCompile(b *testing.B) {
	dir, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	bin := filepath.Join(dir, "compile")
	if out, err := exec.Command("cc", "-O2", "-o", bin, "./testdata/compile/main.c", "-lsandbox").CombinedOutput(); err != nil {
		b.Fatalf("cc: %v\n%s", err, out)
	}
	root := filepath.Join(dir, "root")
	text, err := Render(Spec{Root: root, StateRoot: dir, IPCPrefix: IPCPrefixFor(root)})
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct{ name, text string }{
		{"container", text},
		{"without-system", strings.Replace(text, "(import \"system.sb\")\n", "", 1)},
		{"system-only", "(version 1)\n(deny default)\n(import \"system.sb\")\n"},
		{"allow-default", "(version 1)\n(allow default)\n"},
	} {
		b.Run(c.name, func(b *testing.B) {
			p := filepath.Join(dir, c.name+".sb")
			if err := os.WriteFile(p, []byte(c.text), 0o600); err != nil {
				b.Fatal(err)
			}
			out, err := exec.Command(bin, p, strconv.Itoa(b.N)).Output()
			if err != nil {
				b.Fatalf("compile %s: %v", c.name, err)
			}
			ns, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(ns, "ns/compile")
		})
	}
}
