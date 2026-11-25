package image

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// The store keeps each layer it unpacks once, by diff_id, under layers/:
// <hex>/ holds the layer's entries alone, its whiteouts as the ".wh." files
// that name them, and <hex>.json what the store knows of it (layerMeta).
// An image's root is the cached layers laid over an empty tree in order
// (MergeLayer), each directory the tree lacks one clone of the layer's
// subtree, so images that share layers share their blocks on APFS.
//
// A layer with a hard link to a file of a layer below it cannot be
// unpacked alone; the store records that and applies such a layer onto
// each root that needs it, as a layer applies onto the tree below it.

// layerMeta is what the store records of a cached layer. A record is
// written before the layer's directory takes its name, so a directory
// always has one.
type layerMeta struct {
	// Size is the length of the layer's uncompressed tar stream.
	Size int64 `json:"size"`
	// InPlace marks a layer that applies onto the tree below it alone, and
	// has no directory.
	InPlace bool `json:"inPlace,omitempty"`
	// Blobs are the digests of the compressed blobs the store has read
	// this layer from, each checked against the diff_id: another blob
	// that claims the diff_id is checked before the cache stands for it.
	Blobs []string `json:"blobs,omitempty"`
}

func (s *Store) layersDir() string              { return filepath.Join(s.dir, "layers") }
func (s *Store) layerDir(d v1.Hash) string      { return filepath.Join(s.layersDir(), d.Hex) }
func (s *Store) layerMetaPath(d v1.Hash) string { return filepath.Join(s.layersDir(), d.Hex+".json") }

// cachedLayer reads the record of a layer the store holds.
func (s *Store) cachedLayer(d v1.Hash) (layerMeta, bool) {
	var m layerMeta
	b, err := os.ReadFile(s.layerMetaPath(d))
	if err != nil || json.Unmarshal(b, &m) != nil {
		return layerMeta{}, false
	}
	if !m.InPlace {
		if fi, err := os.Stat(s.layerDir(d)); err != nil || !fi.IsDir() {
			return layerMeta{}, false
		}
	}
	return m, true
}

// layLayer lays the layer l, whose diff_id is d, onto the root dst from the
// cache, unpacking it into the cache first if need be, and returns the
// length of its uncompressed stream. A layer the cache cannot hold applies
// onto dst directly.
func (s *Store) layLayer(l v1.Layer, d v1.Hash, dst string) (int64, error) {
	m, ok := s.cachedLayer(d)
	if !ok {
		var err error
		if m, ok, err = s.cacheLayer(l, d); err != nil || !ok {
			// Applying the layer reports what made it unfit for the cache,
			// if anything does.
			return measure(l, dst, true)
		}
	}
	if m.InPlace {
		return measure(l, dst, true)
	}
	dg, err := l.Digest()
	if err != nil {
		return 0, err
	}
	if !slices.Contains(m.Blobs, dg.String()) {
		if _, err := measure(l, "", false); err != nil {
			return 0, err
		}
		m.Blobs = append(m.Blobs, dg.String())
		// A record that fails to take the blob costs a check next time.
		s.writeLayerMeta(d, m)
	}
	return m.Size, MergeLayer(s.layerDir(d), dst, s.clone)
}

// writeLayerMeta records m for the layer d, through a temporary file.
func (s *Store) writeLayerMeta(d v1.Hash, m layerMeta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	rec, err := os.CreateTemp(s.layersDir(), ".tmp-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(rec.Name())
	_, err = rec.Write(b)
	if cerr := rec.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(rec.Name(), s.layerMetaPath(d))
}

// cacheLayer unpacks l into the cache under its diff_id d, checking the
// stream against d as it reads it. ok is false where the layer cannot be
// cached: its content is not d.
func (s *Store) cacheLayer(l v1.Layer, d v1.Hash) (m layerMeta, ok bool, err error) {
	if d.Algorithm != "sha256" {
		return layerMeta{}, false, nil
	}
	if err := os.MkdirAll(s.layersDir(), 0o700); err != nil {
		return layerMeta{}, false, err
	}
	rc, err := uncompressed(l)
	if err != nil {
		return layerMeta{}, false, err
	}
	defer rc.Close()
	tmp, err := os.MkdirTemp(s.layersDir(), ".tmp-")
	if err != nil {
		return layerMeta{}, false, err
	}
	defer os.RemoveAll(tmp)
	h := sha256.New()
	counted := &countingReader{r: io.TeeReader(rc, h)}
	// The scan reads the stream's headers beside the extraction, for
	// hard links to files the layer does not hold.
	pr, pw := io.Pipe()
	below := make(chan bool, 1)
	go func() { below <- linksBelow(pr) }()
	stream := io.TeeReader(counted, pw)
	xerr := ExtractLayer(tmp, stream)
	if xerr == nil {
		_, xerr = io.Copy(io.Discard, stream)
	}
	pw.Close()
	inPlace := <-below
	switch {
	case inPlace:
		// Read the rest for the record's size and the digest check.
		if _, err := io.Copy(io.Discard, counted); err != nil {
			return layerMeta{}, false, err
		}
	case xerr != nil:
		return layerMeta{}, false, xerr
	}
	if hex.EncodeToString(h.Sum(nil)) != d.Hex {
		return layerMeta{}, false, nil
	}
	dg, err := l.Digest()
	if err != nil {
		return layerMeta{}, false, err
	}
	m = layerMeta{Size: counted.n, InPlace: inPlace, Blobs: []string{dg.String()}}
	if err := s.writeLayerMeta(d, m); err != nil {
		return layerMeta{}, false, err
	}
	if inPlace {
		return m, true, nil
	}
	if err := os.Rename(tmp, s.layerDir(d)); err != nil {
		// Another unpack of the same layer took the name first.
		if _, ok := s.cachedLayer(d); ok {
			return m, true, nil
		}
		return layerMeta{}, false, err
	}
	return m, true, nil
}

// linksBelow reads a layer's tar stream through and reports whether it
// holds a hard link to a file it does not hold itself, which lies in a
// layer below it.
func linksBelow(r io.Reader) bool {
	defer io.Copy(io.Discard, r)
	tr := tar.NewReader(r)
	held := map[string]bool{}
	found := false
	for {
		h, err := tr.Next()
		if err != nil {
			return found
		}
		name := path.Clean("/" + h.Name)
		if h.Typeflag == tar.TypeLink && !held[path.Clean("/"+h.Linkname)] {
			found = true
		}
		held[name] = true
	}
}

// collectLayersLocked removes the cached layers no stored image's config
// names, and what unpacks that did not finish left, and returns the bytes
// they took on disk.
func (s *Store) collectLayersLocked() (uint64, error) {
	keep := map[string]bool{}
	for _, img := range s.idx {
		for _, d := range img.Config.RootFS.DiffIDs {
			keep[d.Hex] = true
		}
	}
	entries, err := os.ReadDir(s.layersDir())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var freed uint64
	for _, e := range entries {
		if keep[strings.TrimSuffix(e.Name(), ".json")] {
			continue
		}
		p := filepath.Join(s.layersDir(), e.Name())
		u := DiskUsage(p)
		if err := os.RemoveAll(p); err != nil {
			return freed, err
		}
		freed += u.Bytes
	}
	return freed, nil
}

// layerOfBlob finds the cached layer the store has read from the
// compressed blob of digest blob, by the records that name it.
func (s *Store) layerOfBlob(blob string) (v1.Hash, layerMeta, bool) {
	entries, err := os.ReadDir(s.layersDir())
	if err != nil {
		return v1.Hash{}, layerMeta{}, false
	}
	for _, e := range entries {
		hex, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasPrefix(hex, ".") {
			continue
		}
		d := v1.Hash{Algorithm: "sha256", Hex: hex}
		if m, ok := s.cachedLayer(d); ok && slices.Contains(m.Blobs, blob) {
			return d, m, true
		}
	}
	return v1.Hash{}, layerMeta{}, false
}

// CloneCachedLayer clones the cached layer the store read from the
// compressed blob of digest blob to dst, which MUST NOT exist: the layer's
// entries alone, its whiteouts as the files that name them, as
// ExtractLayer leaves them. It returns the layer's diff_id and the length
// of its uncompressed stream; ok is false where the store holds no such
// layer apart, and dst is not made.
func (s *Store) CloneCachedLayer(blob, dst string) (diffID string, size int64, ok bool, err error) {
	// Shared with the collector, which removes the layers no image names.
	s.blobs.RLock()
	defer s.blobs.RUnlock()
	d, m, ok := s.layerOfBlob(blob)
	if !ok || m.InPlace {
		return "", 0, false, nil
	}
	if err := s.clone(s.layerDir(d), dst); err != nil {
		return "", 0, false, err
	}
	return d.String(), m.Size, true, nil
}

// knownSize is the length of the uncompressed stream of the layer l, whose
// diff_id is d, where the cache has read it from this very blob.
func (s *Store) knownSize(l v1.Layer, d v1.Hash) (int64, bool) {
	m, ok := s.cachedLayer(d)
	if !ok {
		return 0, false
	}
	dg, err := l.Digest()
	if err != nil || !slices.Contains(m.Blobs, dg.String()) {
		return 0, false
	}
	return m.Size, true
}
