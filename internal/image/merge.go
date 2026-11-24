package image

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MergeLayer lays the layer whose entries dir holds, with its whiteouts
// kept as the ".wh." files that name them (as Extract leaves them), over
// the tree at dst. What the whiteouts name goes from dst first, as they
// hide the layers below and nothing of their own layer, and the entries
// follow. A directory dst lacks comes as one clone of the layer's whole
// subtree, so on APFS the tree shares the layer's blocks; a directory both
// hold takes the layer's mode, owner and modification time (CopyDirMeta).
// clone copies a file, link or whole tree to a path that does not exist:
// CloneTree on macOS, a copying stand-in elsewhere.
func MergeLayer(dir, dst string, clone func(src, dst string) error) error {
	var marks []string
	type dirTime struct {
		rel   string
		mtime time.Time
	}
	var dirs []dirTime
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			dirs = append(dirs, dirTime{rel, fi.ModTime()})
			return nil
		}
		if !strings.HasPrefix(d.Name(), whiteoutPrefix) {
			return nil
		}
		marks = append(marks, rel)
		// A whiteout under what dst holds as a symbolic link names
		// nothing of the tree, and removing through the link would reach
		// outside it.
		if err := checkParents(dst, filepath.ToSlash(rel)); err != nil {
			return nil
		}
		hidden, err := whiteoutTarget(d.Name())
		if err != nil {
			return err
		}
		parent := filepath.Join(dst, filepath.Dir(rel))
		if hidden != "" {
			return os.RemoveAll(filepath.Join(parent, hidden))
		}
		if fi, err := os.Lstat(parent); err != nil || !fi.IsDir() {
			return nil
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(parent, e.Name())); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		if strings.HasPrefix(d.Name(), whiteoutPrefix) {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		fi, lerr := os.Lstat(target)
		if d.IsDir() && lerr == nil && fi.IsDir() {
			return CopyDirMeta(p, target)
		}
		if lerr == nil {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		} else if !errors.Is(lerr, os.ErrNotExist) {
			return lerr
		}
		if err := clone(p, target); err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		// The subtree came whole, the marks inside it too.
		for _, m := range marks {
			if strings.HasPrefix(m, rel+string(filepath.Separator)) {
				if err := os.Remove(filepath.Join(dst, m)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
		return filepath.SkipDir
	})
	if err != nil {
		return err
	}
	// Merging touched the directories; each takes its time from the layer.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chtimes(filepath.Join(dst, dirs[i].rel), time.Time{}, dirs[i].mtime); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
