package image

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func layer(t *testing.T, es ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body))}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return &buf
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestApplyLayersWithWhiteouts(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ApplyLayer(dir, layer(t,
		entry{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "etc/a", body: "a"},
		entry{name: "etc/b", body: "b"},
		entry{name: "opt/x/1", body: "1"},
		entry{name: "bin/tool", body: "#", mode: 0o4755},
		entry{name: "bin/alias", typ: tar.TypeSymlink, link: "tool"},
		entry{name: "bin/hard", typ: tar.TypeLink, link: "bin/tool"},
	)))
	must(ApplyLayer(dir, layer(t,
		entry{name: "etc/.wh.a"},
		entry{name: "opt/x/.wh..wh..opq"},
		entry{name: "opt/x/2", body: "2"},
	)))
	if exists(filepath.Join(dir, "etc/a")) {
		t.Error("whiteout did not remove etc/a")
	}
	if !exists(filepath.Join(dir, "etc/b")) {
		t.Error("whiteout removed a sibling")
	}
	if exists(filepath.Join(dir, "opt/x/1")) || !exists(filepath.Join(dir, "opt/x/2")) {
		t.Error("opaque whiteout did not reset opt/x")
	}
	fi, err := os.Stat(filepath.Join(dir, "bin/tool"))
	must(err)
	if fi.Mode()&os.ModeSetuid != 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("bin/tool mode %v, want 0755 without setuid", fi.Mode())
	}
	if l, _ := os.Readlink(filepath.Join(dir, "bin/alias")); l != "tool" {
		t.Errorf("symlink target %q", l)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "bin/hard")); string(b) != "#" {
		t.Errorf("hard link content %q", b)
	}
}

func TestApplyLayerRefusesEscapes(t *testing.T) {
	outside := t.TempDir()
	for name, l := range map[string]*bytes.Buffer{
		"hardlink":       layer(t, entry{name: "h", typ: tar.TypeLink, link: "../../etc/passwd"}),
		"symlink parent": layer(t, entry{name: "d", typ: tar.TypeSymlink, link: outside}, entry{name: "d/evil", body: "x"}),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := ApplyLayer(dir, l); err == nil {
				t.Fatal("layer applied")
			}
			if _, err := os.Stat(filepath.Join(outside, "evil")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("layer wrote outside its root")
			}
		})
	}
}

func TestApplyLayerClampsDotDotToRoot(t *testing.T) {
	dir := t.TempDir()
	if err := ApplyLayer(dir, layer(t, entry{name: "../../evil", body: "x"})); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "evil")) {
		t.Fatal("entry was not clamped to the layer root")
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"postgres":                 "postgres:latest",
		"postgres:16":              "postgres:16",
		"docker.io/library/redis":  "redis:latest",
		"ghcr.io/e6qu/fws-pg:16":   "ghcr.io/e6qu/fws-pg:16",
		"localhost:5000/app:1.0.0": "localhost:5000/app:1.0.0",
	} {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
