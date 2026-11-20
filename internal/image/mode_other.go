//go:build !darwin

package image

import (
	"os"
	"syscall"
	"time"
)

// recordMeta records nothing: containers, whose library presents the
// record, run on macOS only.
func recordMeta(path string, mode, onDisk os.FileMode, uid, gid int, fresh bool) error { return nil }

// RecordOwner records nothing: only macOS records owners.
func RecordOwner(path string, uid, gid int) error { return nil }

// RecordedMode is the file's own mode.
func RecordedMode(path string, fi os.FileInfo) os.FileMode { return fi.Mode() & modeBits }

// RecordedOwner reports none: only macOS records owners.
func RecordedOwner(path string) (uid, gid int, ok bool) { return 0, 0, false }

// metaFlagged reports false: only macOS marks files.
func metaFlagged(fi os.FileInfo) bool { return false }

// ChangeTime is fi's inode change time, zero where fi carries none.
func ChangeTime(fi os.FileInfo) time.Time {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	return time.Unix(st.Ctim.Unix())
}

// MarkTree marks nothing: the container library, which reads the mark,
// runs on macOS only.
func MarkTree(dir string) error { return nil }
