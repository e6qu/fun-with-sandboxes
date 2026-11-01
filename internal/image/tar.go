package image

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// WriteTar writes dir as one layer: entries in lexical order, relative
// names, no ownership, so the same tree yields the same bytes.
func WriteTar(dir string, w io.Writer) error { return WriteTarWith(dir, w, nil) }

// WriteTarWith is WriteTar with asEmptyDir naming entries, by path relative
// to dir, to write as empty directories instead: a container's mount points,
// which are symlinks to the host directories behind its volumes.
func WriteTarWith(dir string, w io.Writer, asEmptyDir func(rel string) bool) error {
	tw := tar.NewWriter(w)
	var paths []string
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir {
			paths = append(paths, p)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if asEmptyDir != nil {
			if rel, _ := filepath.Rel(dir, p); asEmptyDir(filepath.ToSlash(rel)) {
				h := &tar.Header{Name: filepath.ToSlash(rel) + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: fi.ModTime(), Format: tar.FormatPAX}
				if err := tw.WriteHeader(h); err != nil {
					return err
				}
				continue
			}
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() && fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		h.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		h.Format = tar.FormatPAX
		h.AccessTime, h.ChangeTime = time.Time{}, time.Time{}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

// BuildDir is where builds keep their working roots, on the same volume as
// the image roots so a finished root moves into place without copying.
func (s *Store) BuildDir() string { return filepath.Join(s.dir, "builds") }
