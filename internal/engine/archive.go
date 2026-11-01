package engine

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// fsView resolves container paths the way the container's own processes see
// them: through its root, with mounts at their destinations.
type fsView struct {
	root   string
	mounts []mountRec
}

func (c *Container) view() fsView {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fsView{root: c.root(), mounts: append([]mountRec(nil), c.rec.Mounts...)}
}

func within(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// contains reports whether a host path belongs to the container: its root or
// one of its mount sources.
func (v fsView) contains(h string) bool {
	if within(h, v.root) {
		return true
	}
	for _, m := range v.mounts {
		if within(h, m.Source) {
			return true
		}
	}
	return false
}

func (v fsView) writable(h string) bool {
	for _, m := range v.mounts {
		if within(h, m.Source) {
			return m.RW
		}
	}
	return within(h, v.root)
}

// mountAt returns the mount whose destination symlink sits at host path h.
func (v fsView) mountAt(h string) (mountRec, bool) {
	for _, m := range v.mounts {
		if filepath.Join(v.root, m.Destination) == h {
			return m, true
		}
	}
	return mountRec{}, false
}

// resolve maps a container path to a host path. Symlinks resolve as the
// kernel resolves them for the container, absolute ones on the host, and a
// result outside the container is refused. followLast also resolves the
// final component.
func (v fsView) resolve(p string, followLast bool) (string, error) {
	clean := path.Clean("/" + p)
	if clean == "/" {
		return v.root, nil
	}
	dir, base := path.Split(clean)
	parent, err := filepath.EvalSymlinks(filepath.Join(v.root, dir))
	if err != nil {
		return "", notFound("Could not find the file %s in container", p)
	}
	if !v.contains(parent) {
		return "", invalid("path %s resolves outside the container", p)
	}
	full := filepath.Join(parent, base)
	fi, err := os.Lstat(full)
	if err != nil {
		return "", notFound("Could not find the file %s in container", p)
	}
	if followLast && fi.Mode()&os.ModeSymlink != 0 {
		if full, err = filepath.EvalSymlinks(full); err != nil {
			return "", notFound("Could not find the file %s in container", p)
		}
		if !v.contains(full) {
			return "", invalid("path %s resolves outside the container", p)
		}
	}
	return full, nil
}

// containerPath maps a host path back to the container's view.
func (v fsView) containerPath(h string) string {
	for _, m := range v.mounts {
		if within(h, m.Source) {
			return path.Join(m.Destination, strings.TrimPrefix(h, m.Source))
		}
	}
	if within(h, v.root) {
		return path.Clean("/" + strings.TrimPrefix(h, v.root))
	}
	return h
}

func (v fsView) stat(p string) (container.PathStat, string, error) {
	h, err := v.resolve(p, false)
	if err != nil {
		return container.PathStat{}, "", err
	}
	src := h
	if m, ok := v.mountAt(h); ok {
		src = m.Source
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return container.PathStat{}, "", err
	}
	name := path.Base(path.Clean("/" + p))
	st := container.PathStat{Name: name, Size: fi.Size(), Mode: fi.Mode(), Mtime: fi.ModTime()}
	if fi.Mode()&os.ModeSymlink != 0 {
		if target, err := filepath.EvalSymlinks(src); err == nil {
			st.LinkTarget = v.containerPath(target)
		}
	}
	return st, src, nil
}

// writeTree archives host path h as name, descending into mount sources
// instead of reporting the engine's mount symlinks.
func (v fsView) writeTree(tw *tar.Writer, h, name string) error {
	if m, ok := v.mountAt(h); ok {
		h = m.Source
	}
	fi, err := os.Lstat(h)
	if err != nil {
		return err
	}
	link := ""
	if fi.Mode()&os.ModeSymlink != 0 {
		if link, err = os.Readlink(h); err != nil {
			return err
		}
	}
	if !fi.Mode().IsRegular() && !fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	hdr, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}
	hdr.Name = name
	if fi.IsDir() {
		hdr.Name += "/"
	}
	hdr.Uname, hdr.Gname = "", ""
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	switch {
	case fi.Mode().IsRegular():
		f, err := os.Open(h)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	case fi.IsDir():
		entries, err := os.ReadDir(h)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if err := v.writeTree(tw, filepath.Join(h, e.Name()), path.Join(name, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// ArchiveStat describes a path inside the container, as HEAD /archive does.
func (e *Engine) ArchiveStat(ref, p string) (container.PathStat, error) {
	c, err := e.Container(ref)
	if err != nil {
		return container.PathStat{}, err
	}
	st, _, err := c.view().stat(p)
	return st, err
}

// ArchiveGet writes a tar of the path to w after calling begin with its stat,
// so the caller can send headers first.
func (e *Engine) ArchiveGet(ref, p string, begin func(container.PathStat), w io.Writer) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	v := c.view()
	st, _, err := v.stat(p)
	if err != nil {
		return err
	}
	h, err := v.resolve(p, false)
	if err != nil {
		return err
	}
	begin(st)
	tw := tar.NewWriter(w)
	if err := v.writeTree(tw, h, st.Name); err != nil {
		return err
	}
	e.publishContainer(c, events.ActionArchivePath, map[string]string{"path": p})
	return tw.Close()
}

// ArchivePut extracts a tar stream into a directory of the container.
func (e *Engine) ArchivePut(ref, p string, noOverwriteDirNonDir bool, r io.Reader) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	v := c.view()
	h, err := v.resolve(p, true)
	if err != nil {
		return err
	}
	if m, ok := v.mountAt(h); ok {
		h = m.Source
	}
	fi, err := os.Stat(h)
	if err != nil {
		return notFound("Could not find the file %s in container", p)
	}
	if !fi.IsDir() {
		return invalid("extraction point is not a directory")
	}
	if !v.writable(h) {
		return conflict("container path %s is on a read-only mount", p)
	}
	if err := image.Extract(h, r, noOverwriteDirNonDir); err != nil {
		var ee *Error
		if errors.As(err, &ee) {
			return err
		}
		return invalid("%v", err)
	}
	e.publishContainer(c, events.ActionExtractToDir, map[string]string{"path": p})
	return nil
}
