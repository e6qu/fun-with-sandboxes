package image

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// WriteTar writes dir as one layer: entries in lexical order, relative
// names, no ownership, so the same tree yields the same bytes.
func WriteTar(dir string, w io.Writer) error {
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

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// AddRoot registers dir, a complete root directory, as an image with cfg.
// The store takes dir over: it moves it into place, or removes it when an
// identical image is already stored.
func (s *Store) AddRoot(dir string, cfg v1.ConfigFile, tags []string) (Image, error) {
	h := sha256.New()
	size := &countingWriter{}
	if err := WriteTar(dir, io.MultiWriter(h, size)); err != nil {
		return Image{}, err
	}
	diffID := v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(h.Sum(nil))}
	cfg.OS, cfg.Architecture = Platform.OS, Platform.Architecture
	cfg.RootFS = v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{diffID}}
	b, err := json.Marshal(cfg)
	if err != nil {
		return Image{}, err
	}
	sum := sha256.Sum256(b)
	id := "sha256:" + hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range tags {
		s.untagLocked(t)
	}
	if existing, ok := s.idx[id]; ok {
		if err := os.RemoveAll(dir); err != nil {
			return Image{}, err
		}
		existing.RepoTags = append(existing.RepoTags, tags...)
		return *existing, s.saveLocked()
	}
	if err := os.Rename(dir, s.Root(id)); err != nil {
		return Image{}, err
	}
	rec := &Image{ID: id, RepoTags: tags, Created: cfg.Created.Time, Size: size.n, Config: cfg}
	s.idx[id] = rec
	return *rec, s.saveLocked()
}

// BuildDir is where builds keep their working roots, on the same volume as
// the image roots so a finished root moves into place without copying.
func (s *Store) BuildDir() string { return filepath.Join(s.dir, "builds") }
