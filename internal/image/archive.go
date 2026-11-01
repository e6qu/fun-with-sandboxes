package image

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// ArchiveEntry is one image of a `docker save` archive and the tags it is
// saved under; an image saved by ID has none.
type ArchiveEntry struct {
	Image v1.Image
	Tags  []string
}

// WriteArchive writes images as `docker save` does since Docker 25: an OCI
// image layout (oci-layout, index.json, blobs) that also carries the
// manifest.json earlier Docker releases read.
func WriteArchive(w io.Writer, entries []ArchiveEntry) error {
	tw := tar.NewWriter(w)
	written := map[v1.Hash]bool{}
	blob := func(h v1.Hash, size int64, open func() (io.ReadCloser, error)) error {
		if written[h] {
			return nil
		}
		written[h] = true
		rc, err := open()
		if err != nil {
			return err
		}
		defer rc.Close()
		if err := tw.WriteHeader(&tar.Header{Name: blobName(h), Mode: 0o444, Size: size, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err = io.CopyN(tw, rc, size)
		return err
	}
	bytesBlob := func(b []byte) func() (io.ReadCloser, error) {
		return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}
	file := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o444, Size: int64(len(b)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "blobs/", Mode: 0o755, Typeflag: tar.TypeDir, Format: tar.FormatPAX}); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "blobs/sha256/", Mode: 0o755, Typeflag: tar.TypeDir, Format: tar.FormatPAX}); err != nil {
		return err
	}
	index := v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: []v1.Descriptor{}}
	type legacy struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	manifest := []legacy{}
	for _, e := range entries {
		raw, err := e.Image.RawManifest()
		if err != nil {
			return err
		}
		m, err := e.Image.Manifest()
		if err != nil {
			return err
		}
		dg, err := e.Image.Digest()
		if err != nil {
			return err
		}
		mt, err := e.Image.MediaType()
		if err != nil {
			return err
		}
		cfg, err := e.Image.RawConfigFile()
		if err != nil {
			return err
		}
		if err := blob(m.Config.Digest, int64(len(cfg)), bytesBlob(cfg)); err != nil {
			return err
		}
		entry := legacy{Config: blobName(m.Config.Digest), RepoTags: []string{}, Layers: []string{}}
		for _, l := range m.Layers {
			layer, err := e.Image.LayerByDigest(l.Digest)
			if err != nil {
				return err
			}
			if err := blob(l.Digest, l.Size, layer.Compressed); err != nil {
				return err
			}
			entry.Layers = append(entry.Layers, blobName(l.Digest))
		}
		if err := blob(dg, int64(len(raw)), bytesBlob(raw)); err != nil {
			return err
		}
		desc := v1.Descriptor{MediaType: mt, Digest: dg, Size: int64(len(raw))}
		if len(e.Tags) == 0 {
			index.Manifests = append(index.Manifests, desc)
		}
		for _, t := range e.Tags {
			d := desc
			d.Annotations = refAnnotations(t)
			index.Manifests = append(index.Manifests, d)
			entry.RepoTags = append(entry.RepoTags, t)
		}
		manifest = append(manifest, entry)
	}
	idx, err := json.Marshal(index)
	if err != nil {
		return err
	}
	mf, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := file("index.json", idx); err != nil {
		return err
	}
	if err := file("manifest.json", mf); err != nil {
		return err
	}
	if err := file("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	return tw.Close()
}

func blobName(h v1.Hash) string { return "blobs/" + h.Algorithm + "/" + h.Hex }
