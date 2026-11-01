package buildkit

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/epoch"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// bindDir returns the directory behind a snapshot's mounts. The snapshotter
// hands out a single bind mount, whose source is the snapshot itself; no
// mount is needed to reach it.
func bindDir(mounts []mount.Mount) (string, func(), error) {
	switch {
	case len(mounts) == 0:
		dir, err := os.MkdirTemp("", "buildkit-empty-")
		if err != nil {
			return "", nil, err
		}
		return dir, func() { os.RemoveAll(dir) }, nil
	case len(mounts) == 1 && (mounts[0].Type == "bind" || mounts[0].Type == "rbind"):
		return mounts[0].Source, func() {}, nil
	}
	return "", nil, fmt.Errorf("mounts %+v: only a single bind mount is supported: %w", mounts, errdefs.ErrNotImplemented)
}

// applier unpacks layers into snapshot directories.
type applier struct{ store content.Provider }

func (a applier) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount, _ ...diff.ApplyOpt) (ocispec.Descriptor, error) {
	root, done, err := bindDir(mounts)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer done()
	ra, err := a.store.ReaderAt(ctx, desc)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("open layer %s: %w", desc.Digest, err)
	}
	defer ra.Close()
	r, err := compression.DecompressStream(content.NewReader(ra))
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer r.Close()
	dg := digest.SHA256.Digester()
	n := &counter{}
	if err := image.ApplyLayer(root, io.TeeReader(r, io.MultiWriter(dg.Hash(), n))); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("apply layer %s: %w", desc.Digest, err)
	}
	// Drain what the tar reader left unread so the digest covers the stream.
	if _, err := io.Copy(io.MultiWriter(dg.Hash(), n), r); err != nil {
		return ocispec.Descriptor{}, err
	}
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Size: n.n, Digest: dg.Digest()}, nil
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// comparer writes the difference between two snapshot directories as a
// layer, with containerd's walking differ's media types, compression and
// labels.
type comparer struct{ store content.Store }

func (s comparer) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (ocispec.Descriptor, error) {
	var cfg diff.Config
	for _, o := range opts {
		if err := o(&cfg); err != nil {
			return ocispec.Descriptor{}, err
		}
	}
	if tm := epoch.FromContext(ctx); tm != nil && cfg.SourceDateEpoch == nil {
		cfg.SourceDateEpoch = tm
	}
	var wopts []archive.WriteDiffOpt
	if cfg.SourceDateEpoch != nil {
		wopts = append(wopts, archive.WithSourceDateEpoch(cfg.SourceDateEpoch))
	}
	kind := compression.Uncompressed
	switch {
	case cfg.Compressor != nil:
		if cfg.MediaType == "" {
			return ocispec.Descriptor{}, errors.New("media type must be explicitly specified when using custom compressor")
		}
		kind = compression.Unknown
	default:
		if cfg.MediaType == "" {
			cfg.MediaType = ocispec.MediaTypeImageLayerGzip
		}
		switch cfg.MediaType {
		case ocispec.MediaTypeImageLayer:
		case ocispec.MediaTypeImageLayerGzip:
			kind = compression.Gzip
		case ocispec.MediaTypeImageLayerZstd:
			kind = compression.Zstd
		default:
			return ocispec.Descriptor{}, fmt.Errorf("unsupported diff media type %s: %w", cfg.MediaType, errdefs.ErrNotImplemented)
		}
	}
	lowerDir, doneLower, err := bindDir(lower)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer doneLower()
	upperDir, doneUpper, err := bindDir(upper)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer doneUpper()

	ref := cfg.Reference
	if ref == "" {
		ref = uniqueRef()
	}
	cw, err := s.store.Writer(ctx, content.WithRef(ref), content.WithDescriptor(ocispec.Descriptor{MediaType: cfg.MediaType}))
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("open content writer: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			cw.Close()
			if cfg.Reference == "" {
				s.store.Abort(ctx, ref)
			}
		}
	}()
	if cfg.Reference != "" {
		if err := cw.Truncate(0); err != nil {
			return ocispec.Descriptor{}, err
		}
	}
	if cfg.Labels == nil {
		cfg.Labels = map[string]string{}
	}
	if kind == compression.Uncompressed {
		dg := digest.SHA256.Digester()
		if err := archive.WriteDiff(ctx, io.MultiWriter(cw, dg.Hash()), lowerDir, upperDir, wopts...); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("write diff: %w", err)
		}
		cfg.Labels[labels.LabelUncompressed] = dg.Digest().String()
	} else {
		var zw io.WriteCloser
		if cfg.Compressor != nil {
			zw, err = cfg.Compressor(cw, cfg.MediaType)
		} else {
			zw, err = compression.CompressStream(cw, kind)
		}
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("open compressed stream: %w", err)
		}
		dg := digest.SHA256.Digester()
		err = archive.WriteDiff(ctx, io.MultiWriter(zw, dg.Hash()), lowerDir, upperDir, wopts...)
		if cerr := zw.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("write compressed diff: %w", err)
		}
		cfg.Labels[labels.LabelUncompressed] = dg.Digest().String()
	}
	dgst := cw.Digest()
	if err := cw.Commit(ctx, 0, dgst, content.WithLabels(cfg.Labels)); err != nil && !errdefs.IsAlreadyExists(err) {
		return ocispec.Descriptor{}, fmt.Errorf("commit diff: %w", err)
	}
	ok = true
	info, err := s.store.Info(ctx, dgst)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if _, has := info.Labels[labels.LabelUncompressed]; !has {
		if info.Labels == nil {
			info.Labels = map[string]string{}
		}
		info.Labels[labels.LabelUncompressed] = cfg.Labels[labels.LabelUncompressed]
		if _, err := s.store.Update(ctx, info, "labels."+labels.LabelUncompressed); err != nil {
			return ocispec.Descriptor{}, err
		}
	}
	return ocispec.Descriptor{MediaType: cfg.MediaType, Size: info.Size, Digest: info.Digest}, nil
}

func uniqueRef() string {
	var b [3]byte
	rand.Read(b[:])
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), base64.URLEncoding.EncodeToString(b[:]))
}
