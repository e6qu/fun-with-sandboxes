package image

import (
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// Clone gives dst an APFS copy-on-write clone of the image root.
func (s *Store) Clone(id, dst string) error { return CloneDir(s.Root(id), dst) }

// CloneDir clones the tree at src to dst, which must not exist.
func CloneDir(src, dst string) error {
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	if err != unix.EXDEV {
		return err
	}
	// A clone stays on its volume: across volumes, as to and from a root
	// on a disk image of its own, the tree is copied, its modes, flags,
	// extended attributes and ACLs with it.
	if out, err := exec.Command("ditto", src, dst).CombinedOutput(); err != nil {
		os.RemoveAll(dst)
		return fmt.Errorf("copy %s across volumes: %v: %s", src, err, out)
	}
	return nil
}

// CloneTree clones the file, link or whole tree at src to dst, which must
// not exist, with its modes, flags and extended attributes.
func CloneTree(src, dst string) error { return CloneDir(src, dst) }

// defaultClone is how the store lays a cached layer into a root.
var defaultClone = CloneTree

// CopyDirMeta gives the directory dst the mode of src and what the image
// store records beside it: the mode and owner a container sees, and the
// flag that marks a file as carrying them.
func CopyDirMeta(src, dst string) error {
	var st unix.Stat_t
	if err := unix.Lstat(src, &st); err != nil {
		return err
	}
	if err := os.Chmod(dst, os.FileMode(st.Mode)&os.ModePerm); err != nil {
		return err
	}
	for _, name := range []string{ModeXattr, ownerXattr} {
		buf := make([]byte, 64)
		n, err := unix.Lgetxattr(src, name, buf)
		if err != nil {
			if err := unix.Lremovexattr(dst, name); err != nil && err != unix.ENOATTR {
				return err
			}
			continue
		}
		if err := unix.Lsetxattr(dst, name, buf[:n], 0); err != nil {
			return err
		}
	}
	return unix.Chflags(dst, int(st.Flags))
}
