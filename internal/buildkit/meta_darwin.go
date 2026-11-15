package buildkit

import (
	"os"

	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// cloneTree clones the file, link or whole tree at src to dst, which must
// not exist, with its modes, flags and extended attributes.
func cloneTree(src, dst string) error { return image.CloneDir(src, dst) }

// copyDirMeta gives the directory dst the mode of src and what the image
// store records beside it: the mode and owner a container sees, and the
// flag that marks a file as carrying them.
func copyDirMeta(src, dst string) error {
	var st unix.Stat_t
	if err := unix.Lstat(src, &st); err != nil {
		return err
	}
	if err := os.Chmod(dst, os.FileMode(st.Mode)&os.ModePerm); err != nil {
		return err
	}
	for _, name := range []string{image.ModeXattr, "com.fws.owner"} {
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
