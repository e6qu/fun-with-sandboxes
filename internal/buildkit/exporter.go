package buildkit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	cacheconfig "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/cache/config"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/exporter"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/exporter/containerimage"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/exporter/containerimage/exptypes"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/session"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/compression"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/leaseutil"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// ExporterMoby is the exporter dockerd registers to put a build's result in
// its image store; buildx's docker driver and `docker build` ask for it.
const ExporterMoby = "moby"

type mobyExporter struct {
	store   *image.Store
	writer  *containerimage.ImageWriter
	content content.Store
	leases  leases.Manager
}

func (e *mobyExporter) Resolve(ctx context.Context, id int, attrs map[string]string) (exporter.ExporterInstance, error) {
	opts := containerimage.ImageCommitOpts{
		RefCfg: cacheconfig.RefConfig{Compression: compression.New(compression.Default)},
	}
	if _, err := opts.Load(ctx, attrs); err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(opts.ImageName, ",") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		norm, err := image.Normalize(n)
		if err != nil {
			return nil, fmt.Errorf("invalid image name %q: %w", n, err)
		}
		names = append(names, norm)
	}
	return &mobyInstance{id: id, attrs: attrs, names: names, opts: opts, e: e}, nil
}

type mobyInstance struct {
	id    int
	attrs map[string]string
	names []string
	opts  containerimage.ImageCommitOpts
	e     *mobyExporter
}

func (m *mobyInstance) ID() int      { return m.id }
func (m *mobyInstance) Name() string { return "exporting to image" }
func (m *mobyInstance) Config() *exporter.Config {
	return exporter.NewConfigWithCompression(m.opts.RefCfg.Compression)
}
func (m *mobyInstance) Type() string             { return ExporterMoby }
func (m *mobyInstance) Attrs() map[string]string { return m.attrs }

// Export writes the result as an OCI image with BuildKit's image writer,
// then stores that image, and a clone of the result's root, in the
// engine's image store. Attestations stay out, as dockerd's moby exporter
// leaves them out of its image store.
func (m *mobyInstance) Export(ctx context.Context, src *exporter.Source, info exporter.ExportBuildInfo) (map[string]string, exporter.FinalizeFunc, exporter.DescriptorReference, error) {
	if len(src.Refs) > 0 {
		return nil, nil, nil, fmt.Errorf("exporting a multi-platform build to the engine's images is not supported")
	}
	src = src.Clone()
	src.Attestations = nil
	opts := m.opts
	opts.SetOCITypesDefault(true)
	opts.SetOCIArtifactDefault(false)

	ctx, done, err := leaseutil.WithLease(ctx, m.e.leases, leaseutil.MakeTemporary)
	if err != nil {
		return nil, nil, nil, err
	}
	defer done(context.WithoutCancel(ctx))

	desc, remotes, err := m.e.writer.Commit(ctx, src, info.SessionID, info.InlineCache, &opts, info.CompatibilityVersion, m.Type())
	if err != nil {
		return nil, nil, nil, err
	}
	var layers content.Provider = m.e.content
	if remotes != nil && remotes.Ref != nil {
		layers = remotes.Ref.Provider
	}
	img, err := contentImage(ctx, m.e.content, layers, *desc)
	if err != nil {
		return nil, nil, nil, err
	}
	root, err := m.cloneRoot(ctx, src, info)
	if err != nil {
		return nil, nil, nil, err
	}
	stored, err := m.e.store.Add(img, m.names, root)
	if err != nil {
		return nil, nil, nil, err
	}
	// Image IDs are config digests, as in dockerd's classic store, whose moby
	// exporter reports the ID as the image's digest too.
	return map[string]string{
		exptypes.ExporterImageDigestKey:       stored.ID,
		exptypes.ExporterImageConfigDigestKey: stored.ID,
		exptypes.ExporterImageNameKey:         strings.Join(m.names, ","),
	}, nil, nil, nil
}

// cloneRoot clones the result's snapshot into the store's build directory,
// so the image's root costs no copy.
func (m *mobyInstance) cloneRoot(ctx context.Context, src *exporter.Source, info exporter.ExportBuildInfo) (string, error) {
	store := m.e.store
	if err := os.MkdirAll(store.BuildDir(), 0o700); err != nil {
		return "", err
	}
	dst, err := os.MkdirTemp(store.BuildDir(), "export-")
	if err != nil {
		return "", err
	}
	if src.Ref == nil {
		return dst, nil
	}
	// MkdirTemp creates the directory; the clone needs it absent.
	os.Remove(dst)
	mountable, err := src.Ref.Mount(ctx, true, session.NewGroup(info.SessionID))
	if err != nil {
		return "", err
	}
	ms, release, err := mountable.Mount()
	if err != nil {
		return "", err
	}
	defer release()
	dir, unbind, err := bindDir(ms)
	if err != nil {
		return "", err
	}
	defer unbind()
	if err := image.CloneDir(dir, dst); err != nil {
		os.RemoveAll(dst)
		return "", err
	}
	return dst, nil
}

// contentImage reads an image manifest from a content store as an OCI
// image, its layers from layers, which fetches what is still lazy.
func contentImage(ctx context.Context, cs content.Provider, layers content.Provider, desc ocispecs.Descriptor) (v1.Image, error) {
	raw, err := content.ReadBlob(ctx, cs, desc)
	if err != nil {
		return nil, err
	}
	m, err := v1.ParseManifest(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return partial.CompressedToImage(&csImage{ctx: ctx, cs: cs, layers: layers, desc: desc, raw: raw, manifest: m})
}

type csImage struct {
	ctx      context.Context
	cs       content.Provider
	layers   content.Provider
	desc     ocispecs.Descriptor
	raw      []byte
	manifest *v1.Manifest
}

func (c *csImage) MediaType() (types.MediaType, error) { return types.MediaType(c.desc.MediaType), nil }
func (c *csImage) RawManifest() ([]byte, error)        { return c.raw, nil }
func (c *csImage) RawConfigFile() ([]byte, error) {
	return content.ReadBlob(c.ctx, c.cs, ociDescriptor(c.manifest.Config))
}

func (c *csImage) LayerByDigest(h v1.Hash) (partial.CompressedLayer, error) {
	if h == c.manifest.Config.Digest {
		return &csLayer{ctx: c.ctx, p: c.cs, desc: c.manifest.Config}, nil
	}
	for _, l := range c.manifest.Layers {
		if l.Digest == h {
			return &csLayer{ctx: c.ctx, p: c.layers, desc: l}, nil
		}
	}
	return nil, fmt.Errorf("manifest %s has no blob %s", c.desc.Digest, h)
}

type csLayer struct {
	ctx  context.Context
	p    content.Provider
	desc v1.Descriptor
}

func (l *csLayer) Digest() (v1.Hash, error)            { return l.desc.Digest, nil }
func (l *csLayer) Size() (int64, error)                { return l.desc.Size, nil }
func (l *csLayer) MediaType() (types.MediaType, error) { return l.desc.MediaType, nil }
func (l *csLayer) Compressed() (io.ReadCloser, error) {
	ra, err := l.p.ReaderAt(l.ctx, ociDescriptor(l.desc))
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{content.NewReader(ra), ra}, nil
}

func ociDescriptor(d v1.Descriptor) ocispecs.Descriptor {
	return ocispecs.Descriptor{MediaType: string(d.MediaType), Digest: digest.Digest(d.Digest.String()), Size: d.Size}
}
