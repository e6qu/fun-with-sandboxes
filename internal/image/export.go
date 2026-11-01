package image

import (
	"io"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// V1Image presents a stored image as an OCI image of one layer: a
// deterministic tar of its root. An image the engine built or committed
// keeps its ID this way; a loaded or pulled one, flattened on arrival, gets
// the ID of its flattened form.
func (s *Store) V1Image(ref string) (v1.Image, Image, error) {
	rec, err := s.Get(ref)
	if err != nil {
		return nil, Image{}, err
	}
	root := s.Root(rec.ID)
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(WriteTar(root, pw)) }()
		return pr, nil
	})
	if err != nil {
		return nil, Image{}, err
	}
	diffID, err := layer.DiffID()
	if err != nil {
		return nil, Image{}, err
	}
	cfg := rec.Config.DeepCopy()
	if len(cfg.RootFS.DiffIDs) != 1 || cfg.RootFS.DiffIDs[0] != diffID {
		// A loaded or pulled image's config names its original layers.
		cfg.RootFS = v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{diffID}}
		cfg.History = []v1.History{{Created: cfg.Created, CreatedBy: "flattened by fun-with-sandboxes"}}
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, Image{}, err
	}
	// The stored config replaces the generated one byte for byte, so a
	// built image keeps its ID.
	if img, err = mutate.ConfigFile(img, cfg); err != nil {
		return nil, Image{}, err
	}
	return img, rec, nil
}
