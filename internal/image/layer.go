package image

import (
	"archive/tar"
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/gzip"
	"golang.org/x/sys/unix"
)

const (
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
)

// ModeXattr records a file's mode where extraction kept more on disk: the
// owner's read and write on files and search on directories, which the
// engine's user, and a container's root, needs, and the setuid, setgid and
// sticky bits, which the disk does not keep.
const ModeXattr = "com.fws.mode"

// ApplyLayer extracts one image layer onto dir, honouring OCI whiteouts. The
// engine runs unprivileged, so it records, rather than sets, owners and
// setuid, setgid and sticky bits (see recordMeta), and refuses entries that
// would land outside dir. Files, links and directories keep the layer's
// modification times.
func ApplyLayer(dir string, r io.Reader) error {
	return extract(dir, r, applyWhiteouts, false)
}

// ExtractLayer is ApplyLayer keeping the layer's whiteouts as the files
// that name them, for a caller that acts on them itself.
func ExtractLayer(dir string, r io.Reader) error {
	return extract(dir, r, keepWhiteouts, false)
}

// Extract unpacks a plain tar stream onto dir with ApplyLayer's safety rules,
// treating ".wh." names as ordinary files and giving every file to root.
// With noOverwriteDirNonDir it refuses to replace a directory with a
// non-directory or the reverse, as `docker cp` asks.
func Extract(dir string, r io.Reader, noOverwriteDirNonDir bool) error {
	return extract(dir, r, plainTar, noOverwriteDirNonDir)
}

// streamKind is what a tar stream extract reads is, which decides what
// its ".wh." names and owners mean.
type streamKind uint8

const (
	// plainTar is a tar stream of files: ".wh." names are files, every
	// file is root's.
	plainTar streamKind = iota
	// keepWhiteouts is an image layer whose whiteouts stay the files that
	// name them, with the owners the layer names.
	keepWhiteouts
	// applyWhiteouts is an image layer whose whiteouts remove what they
	// name from the tree below, with the owners the layer names.
	applyWhiteouts
)

func (k streamKind) isLayer() bool { return k != plainTar }

// extraction is the state of one extract: the directories whose times wait
// for the end of the stream, and, for a layer whose whiteouts apply, the
// paths the layer itself wrote, which its opaque whiteouts keep.
type extraction struct {
	dir                  string
	kind                 streamKind
	noOverwriteDirNonDir bool
	dirs                 []dirTime
	written              map[string]bool
	// holding are the directories above the paths in written.
	holding map[string]bool
}

// dirTime is a directory's times, which extract sets once the stream is
// written, as writing the directory's entries changes them.
type dirTime struct {
	rel          string
	atime, mtime time.Time
}

func extract(dir string, r io.Reader, kind streamKind, noOverwriteDirNonDir bool) error {
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
	x := &extraction{dir: filepath.Clean(dir), kind: kind, noOverwriteDirNonDir: noOverwriteDirNonDir}
	if kind == applyWhiteouts {
		x.written, x.holding = map[string]bool{}, map[string]bool{}
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return setDirTimes(dir, x.dirs)
		}
		if err != nil {
			return err
		}
		if err := x.entry(h, tr); err != nil {
			return fmt.Errorf("tar entry %q: %w", h.Name, err)
		}
	}
}

// setDirTimes gives each directory the times its entry named, the last
// entry's where several name one, skipping what later entries replaced or
// put behind a link.
func setDirTimes(dir string, dirs []dirTime) error {
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if checkParents(dir, d.rel) != nil {
			continue
		}
		p := filepath.Join(dir, d.rel)
		if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
			continue
		}
		if err := setTimes(p, d.atime, d.mtime); err != nil {
			return fmt.Errorf("directory %q: %w", d.rel, err)
		}
	}
	return nil
}

// setTimes sets path's access and modification times, not following a
// link; a zero time leaves its time as it is.
func setTimes(path string, atime, mtime time.Time) error {
	if mtime.IsZero() {
		return nil
	}
	if atime.IsZero() {
		atime = mtime
	}
	ts := []unix.Timespec{unix.NsecToTimespec(atime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
}

func (x *extraction) entry(h *tar.Header, r io.Reader) error {
	rel := cleanName(h.Name)
	if rel == "" {
		return nil
	}
	dir := x.dir
	target := filepath.Join(dir, rel)
	if err := checkParents(dir, rel); err != nil {
		return err
	}
	base := path.Base(rel)
	parent := filepath.Dir(target)

	if x.noOverwriteDirNonDir {
		if fi, err := os.Lstat(target); err == nil && fi.IsDir() != (h.Typeflag == tar.TypeDir) {
			if fi.IsDir() {
				return fmt.Errorf("cannot overwrite directory %q with non-directory", "/"+rel)
			}
			return fmt.Errorf("cannot overwrite non-directory %q with directory", "/"+rel)
		}
	}
	if x.kind.isLayer() && strings.HasPrefix(base, whiteoutPrefix) {
		hidden, err := whiteoutTarget(base)
		if err != nil {
			return err
		}
		if x.kind == applyWhiteouts {
			if hidden == "" {
				return x.opaque(parent)
			}
			return os.RemoveAll(filepath.Join(parent, hidden))
		}
	}

	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	x.wrote(target)
	mode := FileModeBits(h.Mode)
	perm := mode.Perm()
	uid, gid := 0, 0
	if x.kind.isLayer() {
		uid, gid = h.Uid, h.Gid
	}

	switch h.Typeflag {
	case tar.TypeDir:
		fresh := true
		if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		} else if err == nil {
			fresh = false
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(target, perm|0o700); err != nil {
			return err
		}
		x.dirs = append(x.dirs, dirTime{rel: rel, atime: h.AccessTime, mtime: h.ModTime})
		return recordMeta(target, mode, perm|0o700, uid, gid, fresh)
	case tar.TypeReg:
		if err := removeNonDir(target); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm|0o600)
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
		if err := os.Chmod(target, perm|0o600); err != nil {
			return err
		}
		if err := recordMeta(target, mode, perm|0o600, uid, gid, true); err != nil {
			return err
		}
		return setTimes(target, h.AccessTime, h.ModTime)
	case tar.TypeSymlink:
		if err := removeNonDir(target); err != nil {
			return err
		}
		if err := os.Symlink(h.Linkname, target); err != nil {
			return err
		}
		if err := recordMeta(target, 0, 0, uid, gid, true); err != nil {
			return err
		}
		return setTimes(target, h.AccessTime, h.ModTime)
	case tar.TypeLink:
		src := cleanName(h.Linkname)
		if src == "" {
			return fmt.Errorf("hard link to the layer root")
		}
		if err := checkParents(dir, src); err != nil {
			return err
		}
		if err := removeNonDir(target); err != nil {
			return err
		}
		// link(2) on macOS follows a symbolic link it is given, which
		// would link a file outside dir in; linkat without
		// AT_SYMLINK_FOLLOW links the symbolic link itself.
		return unix.Linkat(unix.AT_FDCWD, filepath.Join(dir, src), unix.AT_FDCWD, target, 0)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		// Creating device nodes needs root; the host's /dev serves containers.
		return nil
	default:
		return fmt.Errorf("unsupported tar type %q", h.Typeflag)
	}
}

// whiteoutTarget is the name a whiteout entry's base name hides in its
// directory, "" for an opaque whiteout. A name that hides no entry, such
// as ".wh.." would hide the directory's parent, is refused.
func whiteoutTarget(base string) (string, error) {
	if base == opaqueWhiteout {
		return "", nil
	}
	hidden := strings.TrimPrefix(base, whiteoutPrefix)
	if hidden == "" || hidden == "." || hidden == ".." {
		return "", fmt.Errorf("whiteout %q names no entry", base)
	}
	return hidden, nil
}

// opaque empties the directory parent of what the layers below put there,
// keeping what this layer wrote before its opaque whiteout, as dockerd
// does: a whiteout hides lower layers only.
func (x *extraction) opaque(parent string) error {
	if fi, err := os.Lstat(parent); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	} else if !fi.IsDir() {
		return nil
	}
	return filepath.WalkDir(parent, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if p == parent || x.written[p] || x.holding[p] {
			return nil
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

// wrote notes that this layer wrote p, where its opaque whiteouts keep
// what it wrote.
func (x *extraction) wrote(p string) {
	if x.written == nil {
		return
	}
	x.written[p] = true
	for d := filepath.Dir(p); d != x.dir && !x.holding[d]; d = filepath.Dir(d) {
		x.holding[d] = true
	}
}

// cleanName is a tar entry's name relative to the root it extracts to, ""
// for the root itself; ".." at the root stays at the root, as Docker
// clamps it.
func cleanName(name string) string {
	return strings.TrimPrefix(path.Clean("/"+name), "/")
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
