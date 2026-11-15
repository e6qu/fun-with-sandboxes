package image

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/continuity/fs"
)

// DiffOptions tunes WriteDiff.
type DiffOptions struct {
	// SourceDateEpoch caps the modification times the layer carries.
	SourceDateEpoch *time.Time
	// Settled, when set, reports whether a regular file of upper whose
	// size, mode and modification time equal its counterpart's in lower
	// holds the same content too, without reading either: lowerDir and
	// upperDir are the directories that hold the two files.
	Settled func(lower, upper, lowerDir, upperDir os.FileInfo) bool
}

// WriteDiff writes the changes from lower to upper as an uncompressed OCI
// layer: whiteouts for what upper removed, and each entry with the mode
// and owner its container sees (RecordedMode and RecordedOwner, root where
// none is recorded) rather than the widened mode and the engine's user on
// disk, so the layer is the same whichever host wrote it. A file whose
// recorded mode or owner alone changed, as chmod and chown inside a
// container change them, counts as changed.
func WriteDiff(ctx context.Context, w io.Writer, lower, upper string, o DiffOptions) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		var opts []archive.ChangeWriterOpt
		if o.SourceDateEpoch != nil {
			opts = append(opts, archive.WithModTimeUpperBound(*o.SourceDateEpoch))
		}
		cw := archive.NewChangeWriter(pw, upper, opts...)
		err := Changes(ctx, lower, upper, o.Settled, cw.HandleChange)
		if err == nil {
			err = cw.Close()
		}
		pw.CloseWithError(err)
		done <- err
	}()
	err := asRecorded(pr, w, func(name string) (entryMeta, bool) {
		rel := strings.TrimSuffix(name, "/")
		if strings.HasPrefix(path.Base(rel), whiteoutPrefix) {
			return entryMeta{}, false
		}
		p := filepath.Join(upper, filepath.FromSlash(rel))
		fi, err := os.Lstat(p)
		if err != nil {
			return entryMeta{}, false
		}
		m := entryMeta{mode: RecordedMode(p, fi)}
		m.uid, m.gid, _ = RecordedOwner(p)
		return m, true
	})
	pr.CloseWithError(err)
	if werr := <-done; err == nil && werr != nil {
		err = werr
	}
	return err
}

// Changes calls fn for each difference between the trees lower and upper,
// in the order and with the kinds continuity's fs.Changes reports them,
// which containerd's archive.ChangeWriter takes. It differs in what counts
// as a change: the mode and owner a container sees (see WriteDiff) count,
// and a file whose size and modification time match reads as unchanged
// without a look at its content where settled says so.
func Changes(ctx context.Context, lower, upper string, settled func(lower, upper, lowerDir, upperDir os.FileInfo) bool, fn fs.ChangeFunc) error {
	ufi, err := os.Lstat(upper)
	if err != nil {
		return err
	}
	var lfi os.FileInfo
	if lower != "" {
		if lfi, err = os.Lstat(lower); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	d := &differ{ctx: ctx, lower: lower, upper: upper, settled: settled, fn: fn}
	return d.dir("/", lfi, ufi)
}

type differ struct {
	ctx          context.Context
	lower, upper string
	settled      func(lower, upper, lowerDir, upperDir os.FileInfo) bool
	fn           fs.ChangeFunc
}

// dir reports the changes inside the directory rel, which lowerDir and
// upperDir describe in each tree; lowerDir is nil, or no directory, where
// lower holds none there.
func (d *differ) dir(rel string, lowerDir, upperDir os.FileInfo) error {
	if err := d.ctx.Err(); err != nil {
		return err
	}
	upperNames, err := names(filepath.Join(d.upper, rel))
	if err != nil {
		return err
	}
	var lowerNames []string
	if lowerDir != nil && lowerDir.IsDir() {
		if lowerNames, err = names(filepath.Join(d.lower, rel)); err != nil {
			return err
		}
	}
	i, j := 0, 0
	for i < len(lowerNames) || j < len(upperNames) {
		switch {
		case j == len(upperNames) || (i < len(lowerNames) && lowerNames[i] < upperNames[j]):
			if err := d.fn(fs.ChangeKindDelete, path.Join(rel, lowerNames[i]), nil, nil); err != nil {
				return err
			}
			i++
		case i == len(lowerNames) || upperNames[j] < lowerNames[i]:
			p := path.Join(rel, upperNames[j])
			fi, err := os.Lstat(filepath.Join(d.upper, p))
			if err != nil {
				return err
			}
			if err := d.fn(fs.ChangeKindAdd, p, fi, nil); err != nil {
				return err
			}
			if fi.IsDir() {
				if err := d.dir(p, nil, fi); err != nil {
					return err
				}
			}
			j++
		default:
			p := path.Join(rel, upperNames[j])
			lfi, err := os.Lstat(filepath.Join(d.lower, p))
			if err != nil {
				return err
			}
			ufi, err := os.Lstat(filepath.Join(d.upper, p))
			if err != nil {
				return err
			}
			same, err := d.same(p, lfi, ufi, lowerDir, upperDir)
			if err != nil {
				return err
			}
			kind := fs.ChangeKind(fs.ChangeKindModify)
			if same {
				kind = fs.ChangeKindUnmodified
			}
			// The change writer links a hard link it skips to the next
			// one it writes, so it hears of unchanged links too.
			if _, linked := fs.GetLinkInfo(ufi); !same || linked {
				if err := d.fn(kind, p, ufi, nil); err != nil {
					return err
				}
			}
			if ufi.IsDir() {
				if err := d.dir(p, lfi, ufi); err != nil {
					return err
				}
			}
			i++
			j++
		}
	}
	return nil
}

// same reports whether the entry p is the same in both trees.
func (d *differ) same(p string, lfi, ufi, lowerDir, upperDir os.FileInfo) (bool, error) {
	if lfi.Mode() != ufi.Mode() {
		return false, nil
	}
	lp, up := filepath.Join(d.lower, p), filepath.Join(d.upper, p)
	if metaFlagged(lfi) || metaFlagged(ufi) {
		if RecordedMode(lp, lfi) != RecordedMode(up, ufi) {
			return false, nil
		}
		lu, lg, _ := RecordedOwner(lp)
		uu, ug, _ := RecordedOwner(up)
		if lu != uu || lg != ug {
			return false, nil
		}
	}
	if ufi.IsDir() {
		return true, nil
	}
	if lfi.Size() != ufi.Size() || !lfi.ModTime().Equal(ufi.ModTime()) {
		return false, nil
	}
	switch {
	case ufi.Mode()&os.ModeSymlink != 0:
		lt, err := os.Readlink(lp)
		if err != nil {
			return false, err
		}
		ut, err := os.Readlink(up)
		return lt == ut, err
	case !ufi.Mode().IsRegular():
		return true, nil
	// A time finer than a second a tar would have lost: a file of the
	// same size written at the same instant is taken as the same, as
	// continuity takes it.
	case ufi.ModTime().Nanosecond() != 0, ufi.Size() == 0:
		return true, nil
	case d.settled != nil && d.settled(lfi, ufi, lowerDir, upperDir):
		return true, nil
	}
	return sameContent(lp, up)
}

// names lists the directory dir's entries in byte order.
func names(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ns, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(ns)
	return ns, nil
}

func sameContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	ba, bb := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		endA := ea == io.EOF || ea == io.ErrUnexpectedEOF
		endB := eb == io.EOF || eb == io.ErrUnexpectedEOF
		switch {
		case endA && endB:
			return true, nil
		case ea != nil && !endA:
			return false, ea
		case eb != nil && !endB:
			return false, eb
		case endA || endB:
			return false, nil
		}
	}
}
