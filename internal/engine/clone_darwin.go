package engine

import (
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// cloneInto fills the existing, empty directory dst with an APFS clone of
// the tree at src, as a copy would leave it: every directory writable by
// its owner, and no file holding an owner or a mode of the image's (its
// image.MetaFlag cleared, so the container library reads none). A clone
// shares src's blocks, so it takes milliseconds where a copy reads and
// writes every byte. It fails, leaving dst empty, where src and dst are on
// different volumes.
func cloneInto(src, dst string) error {
	tmp := dst + ".clone"
	os.RemoveAll(tmp)
	if err := image.CloneDir(src, tmp); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	err := filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		if st.Flags&image.MetaFlag != 0 && d.Type()&fs.ModeSymlink == 0 {
			if err := unix.Chflags(p, int(st.Flags&^image.MetaFlag)); err != nil {
				return err
			}
		}
		if d.IsDir() && st.Mode&0o700 != 0o700 {
			return unix.Chmod(p, uint32(st.Mode&0o7777|0o700))
		}
		return nil
	})
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(tmp, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
