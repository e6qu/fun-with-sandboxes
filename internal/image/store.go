// Package image keeps the engine's images: each one is stored once as a
// flattened root directory that containers receive as an APFS clone.
package image

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

var ErrNotFound = errors.New("image not found")

// Platform is the only platform the engine runs.
var Platform = v1.Platform{OS: "darwin", Architecture: runtime.GOARCH}

type Image struct {
	ID       string        `json:"id"`
	RepoTags []string      `json:"repoTags"`
	Created  time.Time     `json:"created"`
	Size     int64         `json:"size"`
	Config   v1.ConfigFile `json:"config"`
}

type Store struct {
	dir string
	mu  sync.Mutex
	idx map[string]*Image
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "roots"), 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, idx: map[string]*Image{}}
	b, err := os.ReadFile(s.indexPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.idx); err != nil {
			return nil, fmt.Errorf("image: read index: %w", err)
		}
	}
	return s, nil
}

func (s *Store) indexPath() string { return filepath.Join(s.dir, "images.json") }

// Root returns the image's read-only reference root directory.
func (s *Store) Root(id string) string {
	return filepath.Join(s.dir, "roots", strings.TrimPrefix(id, "sha256:"))
}

// Normalize turns a user reference into the familiar form Docker shows in
// RepoTags, e.g. "postgres" -> "postgres:latest".
func Normalize(ref string) (string, error) {
	r, err := name.ParseReference(ref, name.WithDefaultTag("latest"))
	if err != nil {
		return "", err
	}
	tag, ok := r.(name.Tag)
	if !ok {
		return r.Name(), nil
	}
	repo := tag.Context()
	n := repo.RegistryStr() + "/" + repo.RepositoryStr()
	if repo.RegistryStr() == name.DefaultRegistry {
		n = strings.TrimPrefix(repo.RepositoryStr(), "library/")
	}
	return n + ":" + tag.TagStr(), nil
}

// Get resolves a reference, a full ID, or an unambiguous ID prefix.
func (s *Store) Get(ref string) (Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	img, err := s.getLocked(ref)
	if err != nil {
		return Image{}, err
	}
	return *img, nil
}

func (s *Store) getLocked(ref string) (*Image, error) {
	if img, ok := s.idx[ref]; ok {
		return img, nil
	}
	if n, err := Normalize(ref); err == nil {
		for _, img := range s.idx {
			for _, t := range img.RepoTags {
				if t == n {
					return img, nil
				}
			}
		}
	}
	hex := strings.TrimPrefix(ref, "sha256:")
	if len(hex) >= 4 && isHex(hex) {
		var match *Image
		for id, img := range s.idx {
			if strings.HasPrefix(strings.TrimPrefix(id, "sha256:"), hex) {
				if match != nil {
					return nil, fmt.Errorf("image reference %q is ambiguous", ref)
				}
				match = img
			}
		}
		if match != nil {
			return match, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
}

func (s *Store) List() []Image {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Image, 0, len(s.idx))
	for _, img := range s.idx {
		out = append(out, *img)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Tag adds a tag to an image, moving it off any image that held it.
func (s *Store) Tag(ref, tag string) error {
	n, err := Normalize(tag)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	img, err := s.getLocked(ref)
	if err != nil {
		return err
	}
	s.untagLocked(n)
	img.RepoTags = append(img.RepoTags, n)
	return s.saveLocked()
}

func (s *Store) untagLocked(n string) {
	for _, other := range s.idx {
		kept := other.RepoTags[:0]
		for _, t := range other.RepoTags {
			if t != n {
				kept = append(kept, t)
			}
		}
		other.RepoTags = kept
	}
}

// Remove deletes the image. inUse reports whether a container still needs it.
func (s *Store) Remove(ref string, inUse func(id string) bool) (Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	img, err := s.getLocked(ref)
	if err != nil {
		return Image{}, err
	}
	if inUse(img.ID) {
		return Image{}, fmt.Errorf("conflict: unable to remove image %s: image is being used by a container", ref)
	}
	if err := os.RemoveAll(s.Root(img.ID)); err != nil {
		return Image{}, err
	}
	delete(s.idx, img.ID)
	return *img, s.saveLocked()
}

// Pull fetches the image for the engine's platform from its registry.
func (s *Store) Pull(ref string) (Image, error) {
	r, err := name.ParseReference(ref, name.WithDefaultTag("latest"))
	if err != nil {
		return Image{}, err
	}
	img, err := remote.Image(r, remote.WithPlatform(Platform), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return Image{}, err
	}
	n, err := Normalize(ref)
	if err != nil {
		return Image{}, err
	}
	return s.add(img, []string{n})
}

// Load imports every image in a `docker save` archive.
func (s *Store) Load(r io.Reader) ([]Image, error) {
	tmp, err := os.CreateTemp(s.dir, "load-*.tar")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, r); err != nil {
		return nil, err
	}
	opener := func() (io.ReadCloser, error) { return os.Open(tmp.Name()) }
	manifest, err := readManifest(tmp.Name())
	if err != nil {
		return nil, err
	}
	var out []Image
	for _, m := range manifest {
		var tags []string
		var tagRef *name.Tag
		for _, t := range m.RepoTags {
			n, err := Normalize(t)
			if err != nil {
				return nil, err
			}
			tags = append(tags, n)
			if tagRef == nil {
				tr, err := name.NewTag(t)
				if err != nil {
					return nil, err
				}
				tagRef = &tr
			}
		}
		img, err := tarball.Image(opener, tagRef)
		if err != nil {
			return nil, err
		}
		added, err := s.add(img, tags)
		if err != nil {
			return nil, err
		}
		out = append(out, added)
	}
	return out, nil
}

type manifestEntry struct {
	Config   string
	RepoTags []string
	Layers   []string
}

func readManifest(path string) ([]manifestEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("archive has no manifest.json")
		}
		if err != nil {
			return nil, err
		}
		if filepath.Clean(h.Name) == "manifest.json" {
			var m []manifestEntry
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return nil, fmt.Errorf("manifest.json: %w", err)
			}
			if len(m) == 0 {
				return nil, errors.New("manifest.json lists no images")
			}
			return m, nil
		}
	}
}

func (s *Store) add(img v1.Image, tags []string) (Image, error) {
	cfg, err := img.ConfigFile()
	if err != nil {
		return Image{}, err
	}
	if cfg.OS != Platform.OS || cfg.Architecture != Platform.Architecture {
		return Image{}, fmt.Errorf("image platform (%s/%s) does not match the engine platform (%s/%s)", cfg.OS, cfg.Architecture, Platform.OS, Platform.Architecture)
	}
	digest, err := img.ConfigName()
	if err != nil {
		return Image{}, err
	}
	id := digest.String()

	s.mu.Lock()
	if existing, ok := s.idx[id]; ok {
		for _, t := range tags {
			s.untagLocked(t)
			existing.RepoTags = append(existing.RepoTags, t)
		}
		out := *existing
		err := s.saveLocked()
		s.mu.Unlock()
		return out, err
	}
	s.mu.Unlock()

	stage, err := os.MkdirTemp(filepath.Join(s.dir, "roots"), ".stage-")
	if err != nil {
		return Image{}, err
	}
	defer os.RemoveAll(stage)
	layers, err := img.Layers()
	if err != nil {
		return Image{}, err
	}
	var size int64
	for _, l := range layers {
		rc, err := l.Uncompressed()
		if err != nil {
			return Image{}, err
		}
		err = ApplyLayer(stage, rc)
		rc.Close()
		if err != nil {
			return Image{}, err
		}
		n, err := l.Size()
		if err == nil {
			size += n
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(stage, s.Root(id)); err != nil {
		return Image{}, err
	}
	for _, t := range tags {
		s.untagLocked(t)
	}
	rec := &Image{ID: id, RepoTags: tags, Created: cfg.Created.Time, Size: size, Config: *cfg}
	s.idx[id] = rec
	return *rec, s.saveLocked()
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath())
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
