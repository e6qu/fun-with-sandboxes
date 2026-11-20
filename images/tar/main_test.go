package main

import (
	"archive/zip"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExtractsStdin(t *testing.T) {
	for args, want := range map[string]bool{
		"--no-same-owner -xz -C d": true,
		"xzf -":                    true,
		"-xzf - -C d":              true,
		"-xf-":                     true,
		"--extract --file=-":       true,
		"--get":                    true,
		"-xf a.zip":                false,
		"-xfa.zip":                 false,
		"xf a.zip":                 false,
		"--extract --file a.tar":   false,
		"-czf - d":                 false,
		"-tzf d.tgz":               false,
		"-C x -c d":                false,
	} {
		if got := extractsStdin(split(args)); got != want {
			t.Errorf("%q: %v, want %v", args, got, want)
		}
	}
}

func TestWithFile(t *testing.T) {
	for args, want := range map[string]string{
		"--no-same-owner -xz -C d": "--no-same-owner -xz -C d --file=Z",
		"xzf - -C d":               "xzf Z -C d",
		"-xzf - -C d":              "-xzf Z -C d",
		"-xf-":                     "-xf Z",
		"--extract --file=-":       "--extract --file=Z",
		"--extract --file - -v":    "--extract --file Z -v",
	} {
		if got := withFile(split(args), "Z"); !reflect.DeepEqual(got, split(want)) {
			t.Errorf("%q: %q, want %q", args, got, want)
		}
	}
}

// A zip read from a pipe unpacks with its files' modes, as the Dev
// Containers extension unpacks VS Code's server, and a tar still streams.
func TestZipFromAPipeKeepsModes(t *testing.T) {
	var err error
	if bsdtar, err = exec.LookPath("bsdtar"); err != nil {
		t.Fatalf("bsdtar, which the images' tar fronts: %v", err)
	}
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, mode := range map[string]os.FileMode{"bin/node": 0o755, "data": 0o644} {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(name))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if code := run([]string{"--no-same-owner", "-xz", "-C", dir}, bytes.NewReader(buf.Bytes())); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for name, mode := range map[string]os.FileMode{"bin/node": 0o755, "data": 0o644} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 != mode&0o111 {
			t.Errorf("%s: mode %v, want %v", name, fi.Mode(), mode)
		}
	}
	// A tar on stdin passes through.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o755)
	tarball, err := exec.Command(bsdtar, "-cf", "-", "-C", src, "f").Output()
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if code := run([]string{"-xf", "-", "-C", out}, bytes.NewReader(tarball)); code != 0 {
		t.Fatalf("tar: exit %d", code)
	}
	if b, err := os.ReadFile(filepath.Join(out, "f")); err != nil || string(b) != "x" {
		t.Fatalf("tar: %q, %v", b, err)
	}
}

func split(s string) []string { return strings.Fields(s) }
