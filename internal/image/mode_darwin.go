package image

import (
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// recordMeta records beside path, a file extraction just wrote, what its
// layer gave it that the disk does not hold: mode, setuid, setgid and
// sticky bits included, where it differs from onDisk, and an owner other
// than root. Unless fresh, it also drops what an earlier layer recorded on
// the same directory.
func recordMeta(path string, mode, onDisk os.FileMode, uid, gid int, fresh bool) error {
	marked := false
	if mode != onDisk {
		if err := unix.Lsetxattr(path, ModeXattr, []byte(formatMode(mode)), 0); err != nil {
			return err
		}
		marked = true
	} else if !fresh {
		if err := unix.Lremovexattr(path, ModeXattr); err != nil && !errors.Is(err, unix.ENOATTR) {
			return err
		}
	}
	if uid != 0 || gid != 0 {
		if err := unix.Lsetxattr(path, ownerXattr, []byte(formatOwner(uid, gid)), 0); err != nil {
			return err
		}
		marked = true
	} else if !fresh {
		if err := unix.Lremovexattr(path, ownerXattr); err != nil && !errors.Is(err, unix.ENOATTR) {
			return err
		}
	}
	if !marked {
		return nil
	}
	return Mark(path)
}

// RecordedMode is path's mode as its container sees it, setuid, setgid
// and sticky bits included: the one its image gave it, where extraction
// kept more on disk, else its own.
func RecordedMode(path string, fi os.FileInfo) os.FileMode {
	buf := make([]byte, 16)
	n, err := unix.Lgetxattr(path, ModeXattr, buf)
	if err != nil {
		return fi.Mode() & modeBits
	}
	m, ok := parseMode(string(buf[:n]))
	if !ok {
		return fi.Mode() & modeBits
	}
	return m
}

// RecordedOwner is path's owner as its container sees it, which ok
// reports recorded; root otherwise.
func RecordedOwner(path string) (uid, gid int, ok bool) {
	buf := make([]byte, 32)
	n, err := unix.Lgetxattr(path, ownerXattr, buf)
	if err != nil {
		return 0, 0, false
	}
	return parseOwner(string(buf[:n]))
}

// metaFlagged reports whether fi carries MetaFlag, without which the
// container library reads no recorded mode or owner.
func metaFlagged(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Flags&MetaFlag != 0
}

// ChangeTime is fi's inode change time, zero where fi carries none.
func ChangeTime(fi os.FileInfo) time.Time {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	return time.Unix(st.Ctimespec.Unix())
}
