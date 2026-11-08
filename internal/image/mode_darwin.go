package image

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// recordMode records perm as path's mode where the extractor gave the file
// more than perm on disk, for the container library to present.
func recordMode(path string, perm, onDisk os.FileMode) error {
	if perm == onDisk {
		return nil
	}
	return unix.Lsetxattr(path, ModeXattr, []byte(strconv.FormatUint(uint64(perm), 8)), 0)
}

// RecordedMode is path's mode as its container sees it: the one its image
// gave it, where extraction kept more on disk, else its own.
func RecordedMode(path string, fi os.FileInfo) os.FileMode {
	buf := make([]byte, 16)
	n, err := unix.Lgetxattr(path, ModeXattr, buf)
	if err != nil {
		return fi.Mode().Perm()
	}
	m, err := strconv.ParseUint(string(buf[:n]), 8, 32)
	if err != nil {
		return fi.Mode().Perm()
	}
	return os.FileMode(m) & os.ModePerm
}
