package image

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
)

// ModeXattr records a file's mode where extraction kept more on disk: the
// owner's read and write on files and search on directories, which the
// engine's user, and a container's root, needs.
const ModeXattr = "com.fws.mode"

// ApplyLayer extracts one image layer onto dir, honouring OCI whiteouts. The
// engine runs unprivileged, so it drops ownership and setuid/setgid bits, and
// refuses entries that would land outside dir.
func ApplyLayer(dir string, r io.Reader) error {
	return extract(dir, r, extractOptions{whiteouts: true})
}

// Extract unpacks a plain tar stream onto dir with ApplyLayer's safety rules,
// treating ".wh." names as ordinary files. With noOverwriteDirNonDir it
// refuses to replace a directory with a non-directory or the reverse, as
// `docker cp` asks.
func Extract(dir string, r io.Reader, noOverwriteDirNonDir bool) error {
	return extract(dir, r, extractOptions{noOverwriteDirNonDir: noOverwriteDirNonDir})
}

type extractOptions struct {
	whiteouts            bool
	noOverwriteDirNonDir bool
}

func extract(dir string, r io.Reader, o extractOptions) error {
	br := bufio.NewReader(r)
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	} else {
		r = br
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := applyEntry(dir, h, tr, o); err != nil {
			return fmt.Errorf("tar entry %q: %w", h.Name, err)
		}
	}
}

func applyEntry(dir string, h *tar.Header, r io.Reader, o extractOptions) error {
	rel, err := cleanName(h.Name)
	if err != nil {
		return err
	}
	if rel == "" {
		return nil
	}
	target := filepath.Join(dir, rel)
	if err := checkParents(dir, rel); err != nil {
		return err
	}
	base := path.Base(rel)
	parent := filepath.Dir(target)

	if o.noOverwriteDirNonDir {
		if fi, err := os.Lstat(target); err == nil && fi.IsDir() != (h.Typeflag == tar.TypeDir) {
			if fi.IsDir() {
				return fmt.Errorf("cannot overwrite directory %q with non-directory", "/"+rel)
			}
			return fmt.Errorf("cannot overwrite non-directory %q with directory", "/"+rel)
		}
	}
	switch {
	case !o.whiteouts:
	case base == opaqueWhiteout:
		entries, err := os.ReadDir(parent)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(parent, e.Name())); err != nil {
				return err
			}
		}
		return nil
	case strings.HasPrefix(base, whiteoutPrefix):
		return os.RemoveAll(filepath.Join(parent, strings.TrimPrefix(base, whiteoutPrefix)))
	}

	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	mode := os.FileMode(h.Mode) & os.ModePerm

	switch h.Typeflag {
	case tar.TypeDir:
		if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(target, mode|0o700); err != nil {
			return err
		}
		return recordMode(target, mode, mode|0o700)
	case tar.TypeReg:
		if err := removeNonDir(target); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Chmod(target, mode|0o600); err != nil {
			return err
		}
		return recordMode(target, mode, mode|0o600)
	case tar.TypeSymlink:
		if err := removeNonDir(target); err != nil {
			return err
		}
		return os.Symlink(h.Linkname, target)
	case tar.TypeLink:
		src, err := cleanName(h.Linkname)
		if err != nil {
			return err
		}
		if err := checkParents(dir, src); err != nil {
			return err
		}
		if err := removeNonDir(target); err != nil {
			return err
		}
		return os.Link(filepath.Join(dir, src), target)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		// Creating device nodes needs root; the host's /dev serves containers.
		return nil
	default:
		return fmt.Errorf("unsupported tar type %q", h.Typeflag)
	}
}

func cleanName(name string) (string, error) {
	c := path.Clean("/" + name)
	if c == "/" {
		return "", nil
	}
	rel := strings.TrimPrefix(c, "/")
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return "", fmt.Errorf("path escapes layer root")
		}
	}
	return rel, nil
}

// checkParents refuses to write through a symlinked ancestor, which would let
// a layer place files outside dir.
func checkParents(dir, rel string) error {
	cur := dir
	parts := strings.Split(rel, "/")
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("parent %q is a symlink", strings.TrimPrefix(cur, dir))
		}
	}
	return nil
}

func removeNonDir(p string) error {
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return os.RemoveAll(p)
	}
	return os.Remove(p)
}
