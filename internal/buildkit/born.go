package buildkit

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// bornStamp is the file that records when the snapshot whose directory is
// dir finished cloning its parent's tree, by its modification time, and
// which snapshot that parent was, by its content. Every file the clone
// made changed last before then: a file changed after has a later inode
// change time, which no unprivileged write can set back.
func bornStamp(dir string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(dir)), "born", filepath.Base(dir))
}

// stampBorn writes the born stamp of the snapshot at dir, just cloned from
// the tree of the snapshot parent.
func stampBorn(dir, parent string) error {
	p := bornStamp(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(parent), 0o600)
}

// viewOf starts the born stamp of a view, before its parent.
const viewOf = "view:"

// behindView is the directory holding the tree a view at dir was cloned
// from, which the view's own equals for good: its parent's snapshot, or
// for a parent that holds one layer alone, that parent's whole tree. It
// is dir itself for a snapshot that is no view. BuildKit hands a diff
// views of the two snapshots, clones made a moment before, whose files
// say nothing of when the snapshots' own last changed.
func behindView(dir string) string {
	_, src, ok := readBorn(dir)
	if !ok || !strings.HasPrefix(src, viewOf) {
		return dir
	}
	parent := filepath.Join(filepath.Dir(dir), strings.TrimPrefix(src, viewOf))
	if isDelta(parent) {
		parent = filepath.Join(filepath.Dir(filepath.Dir(dir)), "full", filepath.Base(parent))
	}
	if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
		return dir
	}
	return parent
}

func readBorn(dir string) (at time.Time, parent string, ok bool) {
	p := bornStamp(dir)
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}, "", false
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return time.Time{}, "", false
	}
	return fi.ModTime(), string(b), true
}

// settledSince returns, for a diff from the snapshot directory lower to
// upper, what image.DiffOptions.Settled asks: whether a file of upper
// whose size, mode and modification time match lower's is known to hold
// the same content. It is when upper's file came with the clone upper was
// made by, and lower's file is the one that clone copied: lower is the
// snapshot upper was cloned from, or another clone of that snapshot, and
// neither file nor the directory holding it has changed since its clone.
// A parent directory counts, as renaming a file into place changes the
// directory and need not change the file. It returns nil, so that every
// such file's content is compared, where the stamps say nothing of the
// pair.
func settledSince(lower, upper string) func(l, u, lowerDir, upperDir os.FileInfo) bool {
	upperBorn, src, ok := readBorn(upper)
	if !ok {
		return nil
	}
	var lowerBorn time.Time
	switch {
	case filepath.Base(lower) == src && (filepath.Dir(lower) == filepath.Dir(upper) ||
		lower == filepath.Join(filepath.Dir(filepath.Dir(upper)), "full", src)):
		// lower is the snapshot upper was cloned from, or that snapshot's
		// whole tree: a file of lower unchanged since upper's clone is
		// what the clone copied.
		lowerBorn = upperBorn
	default:
		at, lsrc, ok := readBorn(lower)
		if !ok || lsrc != src {
			return nil
		}
		lowerBorn = at
	}
	return func(l, u, lowerDir, upperDir os.FileInfo) bool {
		return before(u, upperBorn) && before(upperDir, upperBorn) && before(l, lowerBorn) && before(lowerDir, lowerBorn)
	}
}

// before reports whether fi's inode last changed strictly before t.
func before(fi os.FileInfo, t time.Time) bool {
	if fi == nil {
		return false
	}
	c := image.ChangeTime(fi)
	return !c.IsZero() && c.Before(t)
}
