package image

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
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

func TestExtendIsContentAddressed(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func() string {
		d, _ := os.MkdirTemp(s.dir, "root-")
		os.MkdirAll(filepath.Join(d, "etc"), 0o755)
		os.WriteFile(filepath.Join(d, "etc", "f"), []byte("same"), 0o644)
		// Modification times are layer content, so identical trees share them.
		when := time.Unix(1700000000, 0)
		for _, p := range []string{filepath.Join(d, "etc", "f"), filepath.Join(d, "etc")} {
			os.Chtimes(p, when, when)
		}
		return d
	}
	var cfg v1.ConfigFile
	cfg.Config.Cmd = []string{"/x"}
	a, err := extendRoot(t, s, "", mk(), cfg, []string{"built:1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := extendRoot(t, s, "", mk(), cfg, []string{"built:2"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("identical roots got different IDs %s and %s", a.ID, b.ID)
	}
	got, err := s.Get("built:1")
	if err != nil || got.ID != a.ID || got.Config.OS != "darwin" || len(got.Config.RootFS.DiffIDs) != 1 {
		t.Fatalf("Get built:1 = %+v, %v", got, err)
	}
	if body, _ := os.ReadFile(filepath.Join(s.Root(a.ID), "etc", "f")); string(body) != "same" {
		t.Fatalf("stored root lacks its file: %q", body)
	}
}

func TestRemoveUntagsBeforeDeleting(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, _ := os.MkdirTemp(s.dir, "root-")
	var cfg v1.ConfigFile
	img, err := extendRoot(t, s, "", d, cfg, []string{"first:1", "second:1"})
	if err != nil {
		t.Fatal(err)
	}
	unused := func(string) bool { return false }
	if _, err := s.Remove(img.ID, false, unused); !errors.Is(err, ErrConflict) {
		t.Fatalf("removing a multiply tagged image by ID without force: %v", err)
	}
	r, err := s.Remove("second:1", false, func(string) bool { return true })
	if err != nil || r.Deleted || len(r.Untagged) != 1 || r.Untagged[0] != "second:1" {
		t.Fatalf("removing one of two tags: %+v, %v", r, err)
	}
	if _, err := s.Get("first:1"); err != nil {
		t.Fatalf("the other tag went with it: %v", err)
	}
	if _, err := s.Remove("first:1", false, func(string) bool { return true }); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting an image a container uses: %v", err)
	}
	r, err = s.Remove("first:1", false, unused)
	if err != nil || !r.Deleted {
		t.Fatalf("removing the last tag: %+v, %v", r, err)
	}
	if _, err := os.Stat(s.Root(img.ID)); !os.IsNotExist(err) {
		t.Fatalf("deleted image's root survived: %v", err)
	}
}

// A built image exports with its own ID, and loading the export gives back
// the same root.
func TestV1ImageRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, _ := os.MkdirTemp(s.dir, "root-")
	os.MkdirAll(filepath.Join(d, "srv"), 0o755)
	os.WriteFile(filepath.Join(d, "srv", "app"), []byte("app"), 0o755)
	os.Symlink("srv/app", filepath.Join(d, "app"))
	var cfg v1.ConfigFile
	cfg.Created = v1.Time{Time: time.Unix(1700000000, 0).UTC()}
	cfg.Config.Cmd = []string{"/app"}
	built, err := extendRoot(t, s, "", d, cfg, []string{"built:1"})
	if err != nil {
		t.Fatal(err)
	}
	img, rec, err := s.V1Image("built:1")
	if err != nil || rec.ID != built.ID {
		t.Fatalf("V1Image: %v %v", rec.ID, err)
	}
	if name, err := img.ConfigName(); err != nil || name.String() != built.ID {
		t.Fatalf("exported image ID %v, want the built %s (%v)", name, built.ID, err)
	}
	var buf bytes.Buffer
	ref, _ := name.NewTag("copy:1")
	if err := tarball.Write(ref, img, &buf); err != nil {
		t.Fatal(err)
	}
	other, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := other.Load(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(other.Root(loaded[0].ID), "srv", "app")); string(b) != "app" {
		t.Fatalf("loaded root lacks its file: %q", b)
	}
	if l, _ := os.Readlink(filepath.Join(other.Root(loaded[0].ID), "app")); l != "srv/app" {
		t.Fatalf("loaded symlink %q", l)
	}
}

func TestWriteTarWithEmptiesMountPoints(t *testing.T) {
	root, host := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(host, "secret"), []byte("x"), 0o600)
	os.MkdirAll(filepath.Join(root, "var"), 0o755)
	os.Symlink(host, filepath.Join(root, "var", "data"))
	var buf bytes.Buffer
	if err := WriteTarWith(root, &buf, func(rel string) bool { return rel == "var/data" }); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == "var/data/" && h.Typeflag != tar.TypeDir || h.Linkname != "" {
			t.Fatalf("mount point archived as %+v", h)
		}
	}
}

// A copy of an image the store handed out keeps its tags when the store
// moves one of them to another image: a load reads its images' tags while
// another load may take one of them.
func TestHandedOutImagesKeepTheirTags(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var cfg v1.ConfigFile
	d, _ := os.MkdirTemp(s.dir, "root-")
	img, err := extendRoot(t, s, "", d, cfg, []string{"first:1", "second:1"})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Config.Env = []string{"OTHER=1"}
	d2, _ := os.MkdirTemp(s.dir, "root-")
	if _, err := extendRoot(t, s, "", d2, cfg, []string{"first:1"}); err != nil {
		t.Fatal(err)
	}
	if len(img.RepoTags) != 2 || img.RepoTags[0] != "first:1" || img.RepoTags[1] != "second:1" {
		t.Fatalf("the handed-out copy's tags changed to %v", img.RepoTags)
	}
	if got, err := s.Get("second:1"); err != nil || len(got.RepoTags) != 1 {
		t.Fatalf("the store's record: %+v, %v", got.RepoTags, err)
	}
}
