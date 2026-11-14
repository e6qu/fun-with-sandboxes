package engine

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// cloneInto leaves a volume as a copy of the image's tree would: the files
// there, each directory writable by its owner, no file marked as holding an
// owner or mode of the image's, and nothing beside the volume.
func TestCopyUpClonesAsACopyLeaves(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "vol")
	if err := os.MkdirAll(filepath.Join(src, "ro"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(src, "ro", "data")
	if err := os.WriteFile(file, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := image.Mark(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(src, "ro"), 0o755)
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cloneInto(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "ro", "data"))
	if err != nil || string(b) != "x" {
		t.Fatalf("the cloned file: %q, %v", b, err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(dst, "ro", "data"), &st); err != nil || st.Flags&image.MetaFlag != 0 {
		t.Fatalf("the cloned file keeps the image's mark: flags %#x, %v", st.Flags, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "ro")); err != nil || fi.Mode().Perm()&0o700 != 0o700 {
		t.Fatalf("the cloned directory is not its owner's to write: %v %v", fi.Mode(), err)
	}
	if _, err := os.Stat(dst + ".clone"); !os.IsNotExist(err) {
		t.Fatalf("the clone's staging directory is left: %v", err)
	}
}
