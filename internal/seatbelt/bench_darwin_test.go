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

// BenchmarkApply measures a process entering a container's profile two
// ways: compiling the text itself, as the spawn helper does, and applying
// bytecode another process compiled, which a cache of compiled profiles
// would let every start but a container's first do. Each run then opens a
// file outside the profile and must be denied, so the precompiled
// bytecode is shown to confine as the text does.
func BenchmarkApply(b *testing.B) {
	dir, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	bin := filepath.Join(dir, "apply")
	if out, err := exec.Command("cc", "-O2", "-o", bin, "./testdata/apply/main.c", "-lsandbox").CombinedOutput(); err != nil {
		b.Fatalf("cc: %v\n%s", err, out)
	}
	outside, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		b.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	text, err := Render(Spec{Root: root, StateRoot: dir, IPCPrefix: IPCPrefixFor(root)})
	if err != nil {
		b.Fatal(err)
	}
	profile := filepath.Join(dir, "container.sb")
	if err := os.WriteFile(profile, []byte(text), 0o600); err != nil {
		b.Fatal(err)
	}
	blob := filepath.Join(dir, "container.bin")
	if out, err := exec.Command(bin, "compile", profile, blob).CombinedOutput(); err != nil {
		b.Fatalf("compile: %v\n%s", err, out)
	} else {
		b.Logf("compiled: %s", strings.TrimSpace(string(out)))
	}
	for _, c := range []struct{ name, mode, arg string }{
		{"text", "text", profile},
		{"bytecode", "bytecode", blob},
	} {
		b.Run(c.name, func(b *testing.B) {
			var inside float64
			for i := 0; i < b.N; i++ {
				out, err := exec.Command(bin, c.mode, c.arg, secret).CombinedOutput()
				if err != nil {
					b.Fatalf("%s: %v (exit 3: the file outside the profile was opened)\n%s", c.name, err, out)
				}
				ns, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
				if err != nil {
					b.Fatal(err)
				}
				inside += ns
			}
			b.ReportMetric(inside/float64(b.N), "ns/apply")
		})
	}
}
