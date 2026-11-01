package buildkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/leaseutil"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

const (
	namespace = "buildkit"
	// labelImageID records which engine image a materialised record holds.
	labelImageID = "fun-with-sandboxes.image.id"
)

// localImages lets BuildKit resolve FROM against the engine's images, as
// dockerd's builder resolves against the daemon's. On first use it writes an
// engine image into BuildKit's content store as a one-layer OCI image and
// records it in containerd's image metadata.
type localImages struct {
	engine  *image.Store
	meta    images.Store
	content content.Store
	leases  leases.Manager
}

func withNS(ctx context.Context) context.Context { return namespaces.WithNamespace(ctx, namespace) }

func (l *localImages) Get(ctx context.Context, name string) (images.Image, error) {
	ctx = withNS(ctx)
	img, err := l.engine.Get(name)
	if err != nil {
		return l.meta.Get(ctx, name)
	}
	if rec, err := l.meta.Get(ctx, name); err == nil && rec.Labels[labelImageID] == img.ID {
		return rec, nil
	}
	return l.materialise(ctx, name, img)
}

func (l *localImages) List(ctx context.Context, filters ...string) ([]images.Image, error) {
	return l.meta.List(withNS(ctx), filters...)
}

func (l *localImages) Create(ctx context.Context, img images.Image) (images.Image, error) {
	return l.meta.Create(withNS(ctx), img)
}

func (l *localImages) Update(ctx context.Context, img images.Image, fieldpaths ...string) (images.Image, error) {
	return l.meta.Update(withNS(ctx), img, fieldpaths...)
}

func (l *localImages) Delete(ctx context.Context, name string, opts ...images.DeleteOpt) error {
	return l.meta.Delete(withNS(ctx), name, opts...)
}

func (l *localImages) materialise(ctx context.Context, name string, img image.Image) (images.Image, error) {
	ctx, done, err := leaseutil.WithLease(ctx, l.leases, leaseutil.MakeTemporary)
	if err != nil {
		return images.Image{}, err
	}
	defer done(context.WithoutCancel(ctx))

	ref := "fws-" + digest.Digest(img.ID).Encoded()
	w, err := content.OpenWriter(ctx, l.content, content.WithRef(ref+"-layer"))
	if err != nil {
		return images.Image{}, err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(image.WriteTar(l.engine.Root(img.ID), pw)) }()
	size, err := io.Copy(w, pr)
	if err != nil {
		w.Close()
		return images.Image{}, fmt.Errorf("write layer of %s: %w", name, err)
	}
	layer := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: w.Digest(), Size: size}
	if err := w.Commit(ctx, size, layer.Digest); err != nil && !errdefs.IsAlreadyExists(err) {
		w.Close()
		return images.Image{}, err
	}
	w.Close()

	// The config names the layer just written, not the layer the engine
	// image was loaded from.
	cfg := *img.Config.DeepCopy()
	diffID, err := v1.NewHash(layer.Digest.String())
	if err != nil {
		return images.Image{}, err
	}
	cfg.RootFS = v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{diffID}}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return images.Image{}, err
	}
	config := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.FromBytes(cfgJSON), Size: int64(len(cfgJSON))}
	if err := content.WriteBlob(ctx, l.content, ref+"-config", bytes.NewReader(cfgJSON), config); err != nil {
		return images.Image{}, err
	}
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		Config: config, Layers: []ocispec.Descriptor{layer},
	}
	mJSON, err := json.Marshal(manifest)
	if err != nil {
		return images.Image{}, err
	}
	target := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(mJSON), Size: int64(len(mJSON)),
		Platform: &ocispec.Platform{OS: cfg.OS, Architecture: cfg.Architecture},
	}
	gcRefs := content.WithLabels(map[string]string{
		"containerd.io/gc.ref.content.config": config.Digest.String(),
		"containerd.io/gc.ref.content.l.0":    layer.Digest.String(),
	})
	if err := content.WriteBlob(ctx, l.content, ref+"-manifest", bytes.NewReader(mJSON), target, gcRefs); err != nil {
		return images.Image{}, err
	}
	rec := images.Image{Name: name, Target: target, Labels: map[string]string{labelImageID: img.ID}}
	if _, err := l.meta.Get(ctx, name); err == nil {
		return l.meta.Update(ctx, rec, "target", "labels")
	}
	return l.meta.Create(ctx, rec)
}
