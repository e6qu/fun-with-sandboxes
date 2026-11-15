package image

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// layered is an image for the engine's platform of the given layer tars,
// with a label telling it from others of the same layers.
func layered(t *testing.T, label string, tars ...*bytes.Buffer) v1.Image {
	t.Helper()
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: Platform.OS, Architecture: Platform.Architecture, Config: v1.Config{Labels: map[string]string{"l": label}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range tars {
		raw := b.Bytes()
		l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil })
		if err != nil {
			t.Fatal(err)
		}
		if img, err = mutate.AppendLayers(img, l); err != nil {
			t.Fatal(err)
		}
	}
	return img
}

func cachedLayers(t *testing.T, s *Store) []string {
	t.Helper()
	var out []string
	entries, _ := os.ReadDir(s.layersDir())
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func readAt(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Images that share a layer unpack it once; each root is the layers laid in
// order, whiteouts honoured; and a cached layer goes with the last image
// that names it.
func TestImagesShareCachedLayers(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := layer(t, entry{name: "etc/", typ: tar.TypeDir, mode: 0o755}, entry{name: "etc/os", body: "base"}, entry{name: "etc/gone", body: "x"})
	a := layer(t, entry{name: "etc/.wh.gone"}, entry{name: "app/a", body: "a"})
	b := layer(t, entry{name: "etc/os", body: "b"}, entry{name: "app/b", body: "b"})
	imgA, err := s.Add(layered(t, "a", bytes.NewBuffer(base.Bytes()), a), []string{"a:1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	imgB, err := s.Add(layered(t, "b", bytes.NewBuffer(base.Bytes()), b), []string{"b:1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cachedLayers(t, s)); n != 3 {
		t.Fatalf("%d layers cached, want 3: the shared base once", n)
	}
	ra, rb := s.Root(imgA.ID), s.Root(imgB.ID)
	if readAt(t, ra, "etc/os") != "base" || readAt(t, ra, "app/a") != "a" || exists(filepath.Join(ra, "etc/gone")) || exists(filepath.Join(ra, "etc/.wh.gone")) {
		t.Errorf("a's root: %s", listTree(t, ra))
	}
	if readAt(t, rb, "etc/os") != "b" || readAt(t, rb, "etc/gone") != "x" || exists(filepath.Join(rb, "app/a")) {
		t.Errorf("b's root: %s", listTree(t, rb))
	}
	if imgA.LayerSizes[0] != int64(base.Len()) || imgA.Size != int64(base.Len()+a.Len()) {
		t.Errorf("a's layer sizes %v, size %d", imgA.LayerSizes, imgA.Size)
	}
	unused := func(string) bool { return false }
	ownLayer := DiskUsage(s.layerDir(imgA.Config.RootFS.DiffIDs[1])).Bytes
	r, err := s.Remove("a:1", false, unused)
	if err != nil {
		t.Fatal(err)
	}
	if !imgA.LayeredRoot || r.Freed < ownLayer || ownLayer == 0 {
		t.Errorf("removing a freed %d bytes, its own layer alone takes %d", r.Freed, ownLayer)
	}
	if n := len(cachedLayers(t, s)); n != 2 {
		t.Fatalf("%d layers cached after removing a, want b's 2", n)
	}
	// An image of cached layers alone unpacks from the cache.
	again, err := s.Add(layered(t, "b2", bytes.NewBuffer(base.Bytes()), b), []string{"b:2"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if readAt(t, s.Root(again.ID), "etc/os") != "b" || again.Size != imgB.Size {
		t.Errorf("b2's root: %s, size %d", listTree(t, s.Root(again.ID)), again.Size)
	}
	for _, ref := range []string{"b:1", "b:2"} {
		if _, err := s.Remove(ref, false, unused); err != nil {
			t.Fatal(err)
		}
	}
	if left, _ := os.ReadDir(s.layersDir()); len(left) != 0 {
		t.Fatalf("%d entries left in the layer cache", len(left))
	}
}

// A layer with a hard link to a file of a layer below applies onto the
// root as before, and the store remembers it does.
func TestHardLinkBelowAppliesInPlace(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := layer(t, entry{name: "bin/tool", body: "tool"})
	links := layer(t, entry{name: "bin/alias", typ: tar.TypeLink, link: "bin/tool"}, entry{name: "bin/own", body: "own"}, entry{name: "bin/own2", typ: tar.TypeLink, link: "bin/own"})
	raw := links.Bytes()
	img, err := s.Add(layered(t, "x", base, bytes.NewBuffer(raw)), []string{"x:1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	root := s.Root(img.ID)
	if readAt(t, root, "bin/alias") != "tool" || readAt(t, root, "bin/own2") != "own" {
		t.Fatalf("root: %s", listTree(t, root))
	}
	if n := len(cachedLayers(t, s)); n != 1 {
		t.Fatalf("%d layers cached, want the base's alone", n)
	}
	m, ok := s.cachedLayer(img.Config.RootFS.DiffIDs[1])
	if !ok || !m.InPlace || m.Size != int64(len(raw)) {
		t.Fatalf("the linking layer's record: %+v %v", m, ok)
	}
	if img.LayerSizes[1] != int64(len(raw)) {
		t.Errorf("layer sizes %v", img.LayerSizes)
	}
}

// A layer whose content is not the diff_id its config claims stays out of
// the cache, where it would stand for the layer it claims to be, and its
// image out of the store.
func TestLayerCacheChecksDiffIDs(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	good := layer(t, entry{name: "f", body: "good"})
	evil := layer(t, entry{name: "f", body: "evil"})
	goodImg := layered(t, "good", bytes.NewBuffer(good.Bytes()))
	cfg, err := goodImg.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	liar := layered(t, "liar", evil)
	lcfg, err := liar.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	lcfg = lcfg.DeepCopy()
	lcfg.RootFS.DiffIDs = cfg.RootFS.DiffIDs
	if liar, err = mutate.ConfigFile(liar, lcfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(liar, []string{"liar:1"}, ""); err == nil {
		t.Fatal("stored an image whose layer is not its diff_id")
	}
	if n := len(cachedLayers(t, s)); n != 0 {
		t.Fatalf("%d layers cached of a layer that is not its diff_id", n)
	}
	g, err := s.Add(goodImg, []string{"good:1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if readAt(t, s.Root(g.ID), "f") != "good" {
		t.Fatal("the root holds the wrong content")
	}
	if readAt(t, s.layerDir(cfg.RootFS.DiffIDs[0]), "f") != "good" {
		t.Fatal("the cache holds the wrong content")
	}
	// Once the cache holds the layer, a blob that claims it is still
	// checked.
	lcfg.Config.Labels = map[string]string{"l": "liar2"}
	liar2, err := mutate.ConfigFile(liar, lcfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(liar2, []string{"liar:2"}, ""); err == nil {
		t.Fatal("stored an image whose layer is not its diff_id over the cached layer")
	}
}

// Opening a store clears what unfinished work left.
func TestOpenSweepsLeftovers(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	left := []string{
		filepath.Join(dir, "roots", ".stage-1"),
		filepath.Join(dir, "builds", "b123"),
		filepath.Join(dir, "load-1"),
		filepath.Join(dir, "load-2.tar"),
		filepath.Join(dir, "oci", "blobs", "sha256", ".download-1"),
		filepath.Join(dir, "layers", ".tmp-1"),
	}
	for _, p := range left {
		writeAt(t, filepath.Dir(p), filepath.Base(p), "x")
	}
	keep := filepath.Join(dir, "roots", "abc")
	writeAt(t, keep, "f", "f")
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range left {
		if exists(p) {
			t.Errorf("%s survived the sweep", p)
		}
	}
	if !exists(filepath.Join(keep, "f")) {
		t.Error("the sweep removed a root")
	}
}

// Remove tells those who asked of each image it deletes.
func TestOnRemove(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	img, err := s.Add(layered(t, "x", layer(t, entry{name: "f", body: "f"})), []string{"x:1", "x:2"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	cancel := s.OnRemove(func(id string) { got = append(got, id) })
	defer cancel()
	unused := func(string) bool { return false }
	if _, err := s.Remove("x:1", false, unused); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("untagging reported %v", got)
	}
	if _, err := s.Remove("x:2", false, unused); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != img.ID {
		t.Fatalf("removal reported %v, want %s", got, img.ID)
	}
}
