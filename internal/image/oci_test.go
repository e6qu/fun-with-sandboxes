package image

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// extendRoot stores root as base plus one layer holding root's difference
// from base's root (or from nothing).
func extendRoot(t *testing.T, s *Store, base, root string, cfg v1.ConfigFile, tags []string) (Image, error) {
	t.Helper()
	lower := t.TempDir()
	if base != "" {
		lower = s.Root(base)
	}
	f, err := os.CreateTemp(t.TempDir(), "diff-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	if err := Diff(context.Background(), lower, root, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cfg.History = append(cfg.History, v1.History{CreatedBy: "test layer"})
	return s.Extend(base, []string{f.Name()}, root, Config{ConfigFile: cfg}, tags)
}

// darwinImage is a random image of n layers for the engine's platform.
func darwinImage(t *testing.T, n int64) v1.Image {
	t.Helper()
	img, err := random.Image(256, n)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.DeepCopy()
	cfg.OS, cfg.Architecture = Platform.OS, Platform.Architecture
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func digests(t *testing.T, img v1.Image) []v1.Hash {
	t.Helper()
	m, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var out []v1.Hash
	for _, l := range m.Layers {
		out = append(out, l.Digest)
	}
	return out
}

// A loaded image keeps its layers, manifest and ID through the store and
// out again through a save, and the saved archive is an OCI image layout
// that also carries Docker's manifest.json.
func TestSaveAndLoadKeepLayers(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := darwinImage(t, 3)
	var buf bytes.Buffer
	ref, _ := name.NewTag("layered:1")
	if err := tarball.Write(ref, src, &buf); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(&buf)
	if err != nil {
		t.Fatal(err)
	}
	wantID, _ := src.ConfigName()
	if loaded[0].ID != wantID.String() || len(loaded[0].LayerSizes) != 3 {
		t.Fatalf("loaded %+v, want ID %s with 3 layers", loaded[0], wantID)
	}
	// The store measures the root once, as it unpacks it.
	if u := loaded[0].RootUsage; u == nil || *u != DiskUsage(s.Root(loaded[0].ID)) || u.Inodes < 2 {
		t.Errorf("recorded root usage %+v, on disk %+v", u, DiskUsage(s.Root(loaded[0].ID)))
	}
	stored, _, err := s.V1Image("layered:1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := digests(t, stored), digests(t, src); len(got) != 3 || got[0] != want[0] || got[2] != want[2] {
		t.Fatalf("stored layers %v, want %v", got, want)
	}

	var saved bytes.Buffer
	if err := WriteArchive(&saved, []ArchiveEntry{{Image: stored, Tags: []string{"layered:1"}}}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Extract(dir, bytes.NewReader(saved.Bytes()), false); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"oci-layout", "index.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("the saved archive lacks %s: %v", f, err)
		}
	}
	idx, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	im, _ := idx.IndexManifest()
	if len(im.Manifests) != 1 || im.Manifests[0].Annotations[AnnotationImageName] != "index.docker.io/library/layered:1" || im.Manifests[0].Annotations[AnnotationRefName] != "1" {
		t.Fatalf("index.json lists %+v", im.Manifests)
	}
	storedDigest, _ := stored.Digest()
	if im.Manifests[0].Digest != storedDigest {
		t.Fatalf("index.json names manifest %s, want %s", im.Manifests[0].Digest, storedDigest)
	}

	other, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	again, err := other.Load(bytes.NewReader(saved.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if again[0].ID != loaded[0].ID || again[0].Manifest.Digest != storedDigest || again[0].RepoTags[0] != "layered:1" {
		t.Fatalf("reloaded %+v, want ID %s and manifest %s", again[0], loaded[0].ID, storedDigest)
	}

	// An archive with only manifest.json, as Docker before 25 writes it,
	// loads as well.
	legacy := stripLayout(t, saved.Bytes())
	third, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := third.Load(bytes.NewReader(legacy)); err != nil || got[0].ID != loaded[0].ID {
		t.Fatalf("loading manifest.json alone: %+v, %v", got, err)
	}
}

func stripLayout(t *testing.T, archive []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	tr := tar.NewReader(bytes.NewReader(archive))
	tw := tar.NewWriter(&out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "index.json" || h.Name == "oci-layout" {
			continue
		}
		tw.WriteHeader(h)
		io.Copy(tw, tr)
	}
	tw.Close()
	return out.Bytes()
}

// Extending an image shares its layers and adds one with the changes,
// whiteouts included, so the layers alone rebuild the extended root.
func TestExtendAddsALayerOfChanges(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir() + "/root"
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc", "keep"), []byte("keep"), 0o644)
	os.WriteFile(filepath.Join(root, "etc", "gone"), []byte("gone"), 0o644)
	var cfg v1.ConfigFile
	base, err := extendRoot(t, s, "", root, cfg, []string{"base:1"})
	if err != nil {
		t.Fatal(err)
	}

	next := t.TempDir() + "/next"
	if err := copyTree(s.Root(base.ID), next); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(next, "etc", "gone"))
	os.WriteFile(filepath.Join(next, "etc", "new"), []byte("new"), 0o644)
	child, err := extendRoot(t, s, base.ID, next, base.Config, []string{"child:1"})
	if err != nil {
		t.Fatal(err)
	}
	baseImg, _, _ := s.V1Image("base:1")
	childImg, _, _ := s.V1Image("child:1")
	bl, cl := digests(t, baseImg), digests(t, childImg)
	if len(bl) != 1 || len(cl) != 2 || cl[0] != bl[0] {
		t.Fatalf("child layers %v do not extend base layers %v", cl, bl)
	}
	if mt, _ := childImg.MediaType(); mt != types.OCIManifestSchema1 {
		t.Fatalf("child manifest media type %s", mt)
	}
	if len(child.Config.History) != 2 || len(child.Config.RootFS.DiffIDs) != 2 {
		t.Fatalf("child config history %v, diff IDs %v", child.Config.History, child.Config.RootFS.DiffIDs)
	}

	rebuilt := t.TempDir()
	layers, _ := childImg.Layers()
	for _, l := range layers {
		rc, err := l.Uncompressed()
		if err != nil {
			t.Fatal(err)
		}
		if err := ApplyLayer(rebuilt, rc); err != nil {
			t.Fatal(err)
		}
		rc.Close()
	}
	var names []string
	entries, _ := os.ReadDir(filepath.Join(rebuilt, "etc"))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "keep" || names[1] != "new" {
		t.Fatalf("the layers rebuild etc as %v, want [keep new]", names)
	}
}

// Removing an image removes the blobs no other image needs.
func TestRemoveCollectsBlobs(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir() + "/root"
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc", "f"), []byte("f"), 0o644)
	var cfg v1.ConfigFile
	base, err := extendRoot(t, s, "", root, cfg, []string{"base:1"})
	if err != nil {
		t.Fatal(err)
	}
	next := t.TempDir() + "/next"
	copyTree(s.Root(base.ID), next)
	os.WriteFile(filepath.Join(next, "etc", "g"), []byte("g"), 0o644)
	if _, err := extendRoot(t, s, base.ID, next, base.Config, []string{"child:1"}); err != nil {
		t.Fatal(err)
	}
	blobs := func() int {
		e, _ := os.ReadDir(filepath.Join(s.dir, "oci", "blobs", "sha256"))
		return len(e)
	}
	// Two manifests, two configs, two layers.
	if n := blobs(); n != 6 {
		t.Fatalf("%d blobs stored, want 6", n)
	}
	unused := func(string) bool { return false }
	if _, err := s.Remove("child:1", false, unused); err != nil {
		t.Fatal(err)
	}
	if n := blobs(); n != 3 {
		t.Fatalf("%d blobs after removing the child, want the base's 3", n)
	}
	if _, err := s.Remove("base:1", false, unused); err != nil {
		t.Fatal(err)
	}
	if n := blobs(); n != 0 {
		t.Fatalf("%d blobs after removing every image", n)
	}
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			if err := os.MkdirAll(target, fi.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(target, fi.ModTime(), fi.ModTime())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, b, fi.Mode().Perm()); err != nil {
			return err
		}
		return os.Chtimes(target, fi.ModTime(), fi.ModTime())
	})
}

// The health check's start interval, which go-containerregistry's config
// type drops, survives storing an image, reopening the store, and saving
// and loading it.
func TestHealthStartIntervalKept(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir() + "/root"
	os.MkdirAll(root, 0o755)
	var cfg v1.ConfigFile
	cfg.Config.Healthcheck = &v1.HealthConfig{Test: []string{"CMD", "/probe"}, Interval: time.Minute}
	img, err := s.Extend("", nil, root, Config{ConfigFile: cfg, HealthStartInterval: 3 * time.Second}, []string{"hc:1"})
	if err != nil {
		t.Fatal(err)
	}
	if img.HealthStartInterval != 3*time.Second {
		t.Fatalf("stored start interval %v", img.HealthStartInterval)
	}
	v, _, err := s.V1Image("hc:1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := v.RawConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if got := healthStartInterval(raw); got != 3*time.Second {
		t.Fatalf("the image's config holds start interval %v", got)
	}
	if name, _ := v.ConfigName(); name.String() != img.ID {
		t.Fatalf("config digest %s, image ID %s", name, img.ID)
	}
	if err := validateImage(v); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Get("hc:1"); err != nil || got.HealthStartInterval != 3*time.Second {
		t.Fatalf("after reopening: %v, %v", got.HealthStartInterval, err)
	}
}

func validateImage(img v1.Image) error {
	m, err := img.Manifest()
	if err != nil {
		return err
	}
	raw, err := img.RawConfigFile()
	if err != nil {
		return err
	}
	h, size, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if h != m.Config.Digest || size != m.Config.Size {
		return fmt.Errorf("manifest names config %s (%d bytes), the config is %s (%d bytes)", m.Config.Digest, m.Config.Size, h, size)
	}
	return nil
}
