package image

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Annotations Docker and containerd put on an OCI index's manifest
// descriptors to name the image each one is.
const (
	AnnotationImageName = "io.containerd.image.name"
	AnnotationRefName   = "org.opencontainers.image.ref.name"
)

// layout is the store's OCI image layout: every stored image's manifest,
// config and layers, with index.json listing the images by tag.
func (s *Store) layout() layout.Path { return layout.Path(filepath.Join(s.dir, "oci")) }

func (s *Store) blobPath(h v1.Hash) string {
	return filepath.Join(s.dir, "oci", "blobs", h.Algorithm, h.Hex)
}

func (s *Store) openLayout() error {
	if _, err := os.Stat(filepath.Join(s.dir, "oci", "oci-layout")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := layout.Write(filepath.Join(s.dir, "oci"), empty.Index)
	return err
}

// stored reads an image back from the store's blobs.
func (s *Store) stored(manifest v1.Descriptor) (v1.Image, error) {
	raw, err := os.ReadFile(s.blobPath(manifest.Digest))
	if err != nil {
		return nil, fmt.Errorf("image manifest %s: %w", manifest.Digest, err)
	}
	m, err := v1.ParseManifest(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return partial.CompressedToImage(&blobImage{s: s, desc: manifest, raw: raw, manifest: m})
}

type blobImage struct {
	s        *Store
	desc     v1.Descriptor
	raw      []byte
	manifest *v1.Manifest
	idsMu    sync.Mutex
	ids      []v1.Hash
}

func (b *blobImage) MediaType() (types.MediaType, error) { return b.desc.MediaType, nil }
func (b *blobImage) RawManifest() ([]byte, error)        { return b.raw, nil }
func (b *blobImage) RawConfigFile() ([]byte, error) {
	return os.ReadFile(b.s.blobPath(b.manifest.Config.Digest))
}

func (b *blobImage) LayerByDigest(h v1.Hash) (partial.CompressedLayer, error) {
	if h == b.manifest.Config.Digest {
		return &blobLayer{s: b.s, desc: b.manifest.Config}, nil
	}
	for i, l := range b.manifest.Layers {
		if l.Digest == h {
			ids, err := b.diffIDs()
			if err != nil {
				return nil, err
			}
			return &fsLayer{blobLayer{s: b.s, desc: l}, ids[i]}, nil
		}
	}
	return nil, fmt.Errorf("image %s has no blob %s", b.desc.Digest, h)
}

// diffIDs are the config's rootfs.diff_ids, one for each of the manifest's
// layers.
func (b *blobImage) diffIDs() ([]v1.Hash, error) {
	b.idsMu.Lock()
	defer b.idsMu.Unlock()
	if b.ids != nil {
		return b.ids, nil
	}
	raw, err := b.RawConfigFile()
	if err != nil {
		return nil, err
	}
	cfg, err := v1.ParseConfigFile(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if n, m := len(b.manifest.Layers), len(cfg.RootFS.DiffIDs); n != m {
		return nil, fmt.Errorf("image %s has %d layers but its config lists %d diff_ids", b.desc.Digest, n, m)
	}
	b.ids = cfg.RootFS.DiffIDs
	return b.ids, nil
}

type blobLayer struct {
	s    *Store
	desc v1.Descriptor
}

func (l *blobLayer) Digest() (v1.Hash, error)            { return l.desc.Digest, nil }
func (l *blobLayer) Size() (int64, error)                { return l.desc.Size, nil }
func (l *blobLayer) MediaType() (types.MediaType, error) { return l.desc.MediaType, nil }
func (l *blobLayer) Compressed() (io.ReadCloser, error)  { return os.Open(l.s.blobPath(l.desc.Digest)) }

// fsLayer is a layer blob with the diff_id its image's config gives it,
// which measure checks the layer's content against as it unpacks it.
type fsLayer struct {
	blobLayer
	diffID v1.Hash
}

func (l *fsLayer) DiffID() (v1.Hash, error) { return l.diffID, nil }

// writeIndexLocked rewrites index.json from the image records: a descriptor
// per tag, named as containerd and Docker name them, and one for each
// untagged image.
func (s *Store) writeIndexLocked() error {
	idx := v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: []v1.Descriptor{}}
	ids := make([]string, 0, len(s.idx))
	for id := range s.idx {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		img := s.idx[id]
		if len(img.RepoTags) == 0 {
			idx.Manifests = append(idx.Manifests, img.Manifest)
		}
		for _, t := range img.RepoTags {
			d := img.Manifest
			d.Annotations = refAnnotations(t)
			idx.Manifests = append(idx.Manifests, d)
		}
	}
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return s.layout().WriteFile("index.json", b, 0o600)
}

// refAnnotations names a tagged descriptor the way `docker save` does.
func refAnnotations(tag string) map[string]string {
	t, err := name.NewTag(tag)
	if err != nil {
		return nil
	}
	return map[string]string{AnnotationImageName: t.Name(), AnnotationRefName: t.TagStr()}
}

// collectLocked removes the blobs, and the cached layers, no stored image
// refers to, and returns the bytes they took on disk.
func (s *Store) collectLocked() (uint64, error) {
	keep := map[string]bool{}
	for _, img := range s.idx {
		keep[img.Manifest.Digest.Hex] = true
		// The index a pull resolved a reference to.
		for _, d := range img.RepoDigests {
			if h, err := v1.NewHash(d[strings.LastIndex(d, "@")+1:]); err == nil {
				keep[h.Hex] = true
			}
		}
		raw, err := os.ReadFile(s.blobPath(img.Manifest.Digest))
		if err != nil {
			return 0, err
		}
		m, err := v1.ParseManifest(bytes.NewReader(raw))
		if err != nil {
			return 0, err
		}
		keep[m.Config.Digest.Hex] = true
		for _, l := range m.Layers {
			keep[l.Digest.Hex] = true
		}
	}
	dir := filepath.Join(s.dir, "oci", "blobs", "sha256")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var freed uint64
	for _, e := range entries {
		if !keep[e.Name()] {
			p := filepath.Join(dir, e.Name())
			u := DiskUsage(p)
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return freed, err
			}
			freed += u.Bytes
		}
	}
	n, err := s.collectLayersLocked()
	return freed + n, err
}

// ociLayerType maps a layer media type onto its OCI equivalent, as BuildKit
// does when it writes an OCI manifest over Docker-typed base layers.
func ociLayerType(mt types.MediaType) types.MediaType {
	// Only Docker's layer types have OCI equivalents; every other type
	// stays as it is.
	//exhaustive:ignore
	switch mt {
	case types.DockerLayer:
		return types.OCILayer
	case types.DockerUncompressedLayer:
		return types.OCIUncompressedLayer
	case types.DockerForeignLayer:
		return types.OCIRestrictedLayer
	}
	return mt
}

// Extend stores root, a complete root directory, as an OCI image: the layers
// of the image base (none when base is empty, as for scratch) followed by
// diffs, uncompressed layer tars on disk, under cfg. The store takes root
// over.
func (s *Store) Extend(base string, diffs []string, root string, config Config, tags []string) (Image, error) {
	cfg := config.ConfigFile
	img := mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), types.OCIConfigJSON)
	var adds []mutate.Addendum
	if base != "" {
		b, _, err := s.V1Image(base)
		if err != nil {
			return Image{}, err
		}
		layers, err := b.Layers()
		if err != nil {
			return Image{}, err
		}
		for _, l := range layers {
			mt, err := l.MediaType()
			if err != nil {
				return Image{}, err
			}
			adds = append(adds, mutate.Addendum{Layer: l, MediaType: ociLayerType(mt)})
		}
	}
	for _, p := range diffs {
		l, err := tarball.LayerFromFile(p, tarball.WithMediaType(types.OCILayer))
		if err != nil {
			return Image{}, err
		}
		adds = append(adds, mutate.Addendum{Layer: l, MediaType: types.OCILayer})
	}
	img, err := mutate.Append(img, adds...)
	if err != nil {
		return Image{}, err
	}
	cfg.OS, cfg.Architecture = Platform.OS, Platform.Architecture
	cfg.RootFS = v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{}}
	for _, a := range adds {
		d, err := a.Layer.DiffID()
		if err != nil {
			return Image{}, err
		}
		cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, d)
	}
	if img, err = mutate.ConfigFile(img, &cfg); err != nil {
		return Image{}, err
	}
	if config.HealthStartInterval > 0 {
		if img, err = WithStartInterval(img, config.HealthStartInterval); err != nil {
			return Image{}, err
		}
	}
	return s.Add(img, tags, root)
}

// Diff writes the changes from lower to upper as an uncompressed OCI layer,
// whiteouts for what upper removed, as WriteDiff writes it.
func Diff(ctx context.Context, lower, upper string, w io.Writer) error {
	return WriteDiff(ctx, w, lower, upper, DiffOptions{})
}
