//go:build !darwin

package image

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

var errNoClonefile = errors.New("image: cloning a root requires macOS (APFS clonefile)")

// Clone needs APFS clonefile(2), which only macOS provides.
func (s *Store) Clone(id, dst string) error { return errNoClonefile }

func CloneDir(src, dst string) error { return errNoClonefile }

// CloneTree needs APFS clonefile(2), which only macOS provides.
func CloneTree(src, dst string) error { return errNoClonefile }

// defaultClone copies where there is no clonefile, so the store's logic
// runs, and is tested, off macOS.
var defaultClone = CopyTree

// CopyDirMeta gives the directory dst the mode of src.
func CopyDirMeta(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode().Perm())
}

// CopyTree copies the file, link or whole tree at src to dst, which MUST
// NOT exist, with modes and modification times: a stand-in for CloneTree.
func CopyTree(src, dst string) error {
	var dirs []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			dirs = append(dirs, p)
			return os.Mkdir(target, 0o700)
		case fi.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case fi.Mode().IsRegular():
			if err := copyRegular(p, target, fi.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(target, fi.ModTime(), fi.ModTime())
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		fi, err := os.Lstat(dirs[i])
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, dirs[i])
		target := filepath.Join(dst, rel)
		if err := os.Chmod(target, fi.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Chtimes(target, fi.ModTime(), fi.ModTime()); err != nil {
			return err
		}
	}
	return nil
}

func copyRegular(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
