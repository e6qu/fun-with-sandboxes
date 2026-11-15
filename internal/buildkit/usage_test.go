package buildkit

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A snapshot's usage counts what it added to or changed in its parent's
// tree, by size, time and mode, and nothing its parent holds as it is.
func TestAddedUsage(t *testing.T) {
	parent, child := filepath.Join(t.TempDir(), "p"), filepath.Join(t.TempDir(), "c")
	// Whole seconds, as a layer's files have.
	at := time.Unix(1, 0)
	for _, root := range []string{parent, child} {
		for name, body := range map[string]string{"same": "unchanged", "dir/deep": "below", "grown": "x", "touched": "same size", "mode": "m"} {
			write(t, root, name, body)
			if err := os.Chtimes(filepath.Join(root, name), at, at); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("same", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
	}
	write(t, child, "grown", "xxxx")
	os.Chtimes(filepath.Join(child, "grown"), at, at)
	os.Chtimes(filepath.Join(child, "touched"), at.Add(time.Second), at.Add(time.Second))
	os.Chmod(filepath.Join(child, "mode"), 0o755)
	write(t, child, "dir/new", "12345")
	os.Remove(filepath.Join(child, "link"))
	os.Symlink("grown", filepath.Join(child, "link"))
	u, err := addedUsage(parent, child)
	if err != nil {
		t.Fatal(err)
	}
	// grown (4), touched (9), mode (1), dir/new (5) and the link.
	if u.Inodes != 5 || u.Size != 19 {
		t.Errorf("usage %+v, want 5 entries of 19 bytes", u)
	}
}
