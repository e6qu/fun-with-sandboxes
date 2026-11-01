package buildkit

import (
	"bytes"
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/leaseutil"
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
// engine image's blobs into BuildKit's content store.
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

// materialise writes an engine image into BuildKit's content store as it
// is stored, manifest, config and layers byte for byte, and records it in
// containerd's image metadata.
func (l *localImages) materialise(ctx context.Context, name string, rec image.Image) (images.Image, error) {
	ctx, done, err := leaseutil.WithLease(ctx, l.leases, leaseutil.MakeTemporary)
	if err != nil {
		return images.Image{}, err
	}
	defer done(context.WithoutCancel(ctx))

	img, _, err := l.engine.V1Image(rec.ID)
	if err != nil {
		return images.Image{}, err
	}
	m, err := img.Manifest()
	if err != nil {
		return images.Image{}, err
	}
	labels := map[string]string{"containerd.io/gc.ref.content.config": m.Config.Digest.String()}
	for i, ld := range m.Layers {
		layer, err := img.LayerByDigest(ld.Digest)
		if err != nil {
			return images.Image{}, err
		}
		rc, err := layer.Compressed()
		if err != nil {
			return images.Image{}, err
		}
		err = content.WriteBlob(ctx, l.content, "fws-"+ld.Digest.Hex, rc, ociDescriptor(ld))
		rc.Close()
		if err != nil {
			return images.Image{}, fmt.Errorf("write layer %s of %s: %w", ld.Digest, name, err)
		}
		labels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)] = ld.Digest.String()
	}
	cfg, err := img.RawConfigFile()
	if err != nil {
		return images.Image{}, err
	}
	if err := content.WriteBlob(ctx, l.content, "fws-"+m.Config.Digest.Hex, bytes.NewReader(cfg), ociDescriptor(m.Config)); err != nil {
		return images.Image{}, err
	}
	raw, err := img.RawManifest()
	if err != nil {
		return images.Image{}, err
	}
	target := ociDescriptor(rec.Manifest)
	target.Platform = &ocispec.Platform{OS: rec.Config.OS, Architecture: rec.Config.Architecture}
	if err := content.WriteBlob(ctx, l.content, "fws-"+rec.Manifest.Digest.Hex, bytes.NewReader(raw), target, content.WithLabels(labels)); err != nil {
		return images.Image{}, err
	}
	out := images.Image{Name: name, Target: target, Labels: map[string]string{labelImageID: rec.ID}}
	if _, err := l.meta.Get(ctx, name); err == nil {
		return l.meta.Update(ctx, out, "target", "labels")
	}
	return l.meta.Create(ctx, out)
}
