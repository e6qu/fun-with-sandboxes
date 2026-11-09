package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/continuity/fs"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// The initial root layInit completes from a clone of the image matches the
// root create wrote into: docker diff of a fresh container lists nothing.
func TestLayInitMatchesTheCreatedRoot(t *testing.T) {
	dir := t.TempDir()
	img, root, init := filepath.Join(dir, "img"), filepath.Join(dir, "r"), filepath.Join(dir, "init")
	for _, p := range []string{"etc", "data/db", "usr/lib"} {
		if err := os.MkdirAll(filepath.Join(img, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(img, "etc/hostname"), []byte("image\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/lib", filepath.Join(img, "lib")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{root, init} {
		if err := image.CloneDir(img, d); err != nil {
			t.Fatal(err)
		}
	}
	mounts := []mountRec{{Destination: "/data/db"}, {Destination: "/new/deep/dir"}, {Destination: "/lib/x"}}
	for _, m := range mounts {
		dest := filepath.Join(root, m.Destination)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(dest); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir, dest); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeHostsFiles(root, "box", "", nil, nil, nil, mounts); err != nil {
		t.Fatal(err)
	}
	if err := layInit(root, init, initPaths(mounts)); err != nil {
		t.Fatal(err)
	}
	var changes []string
	if err := fs.Changes(context.Background(), init, root, func(k fs.ChangeKind, p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		changes = append(changes, k.String()+" "+p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(changes) > 0 {
		t.Fatalf("the initial root differs from the created one: %v", changes)
	}
	if b, err := os.ReadFile(filepath.Join(init, "etc/hostname")); err != nil || string(b) != "box\n" {
		t.Fatalf("the initial /etc/hostname: %q, %v", b, err)
	}
}
