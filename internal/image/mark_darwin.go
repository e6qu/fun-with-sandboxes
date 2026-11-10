package image

import (
	"bytes"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// MetaFlag marks a file that holds an owner or a mode of the container's
// in an extended attribute: the container library reads the attributes
// only of a file that has it, as stat returns the flag at no cost.
const MetaFlag = unix.UF_NODUMP

// ownerXattr is the owner the container library records for a file a
// container chowns or a user of its makes.
const ownerXattr = "com.fws.owner"

// Mark sets MetaFlag on path, which it does not follow.
func Mark(path string) error {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return err
	}
	if st.Flags&MetaFlag != 0 {
		return nil
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_SYMLINK|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Fchflags(fd, int(st.Flags|MetaFlag))
}

// MarkTree sets MetaFlag on every file under dir that holds an owner or a
// mode of the container's, as engines before the flag recorded them
// without it.
func MarkTree(dir string) error {
	buf := make([]byte, 4096)
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		n, err := unix.Llistxattr(p, buf)
		if err != nil || n == 0 {
			return nil
		}
		for _, name := range bytes.Split(buf[:n], []byte{0}) {
			if s := string(name); s == ModeXattr || s == ownerXattr {
				return Mark(p)
			}
		}
		return nil
	})
}
