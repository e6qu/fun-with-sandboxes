package buildkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/exporter"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/exporter/containerimage/exptypes"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/session"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// ExporterMoby is the exporter dockerd registers to put a build's result in
// its image store; buildx's docker driver and `docker build` ask for it.
const ExporterMoby = "moby"

type mobyExporter struct{ store *image.Store }

func (e *mobyExporter) Resolve(_ context.Context, id int, attrs map[string]string) (exporter.ExporterInstance, error) {
	var names []string
	for _, n := range strings.Split(attrs[string(exptypes.OptKeyName)], ",") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		norm, err := image.Normalize(n)
		if err != nil {
			return nil, fmt.Errorf("invalid image name %q: %w", n, err)
		}
		names = append(names, norm)
	}
	return &mobyInstance{id: id, attrs: attrs, names: names, store: e.store}, nil
}

type mobyInstance struct {
	id    int
	attrs map[string]string
	names []string
	store *image.Store
}

func (m *mobyInstance) ID() int                  { return m.id }
func (m *mobyInstance) Name() string             { return "exporting to image" }
func (m *mobyInstance) Config() *exporter.Config { return exporter.NewConfig() }
func (m *mobyInstance) Type() string             { return ExporterMoby }
func (m *mobyInstance) Attrs() map[string]string { return m.attrs }

// Export clones the result's root into the engine's image store under the
// frontend's image config.
func (m *mobyInstance) Export(ctx context.Context, src *exporter.Source, info exporter.ExportBuildInfo) (map[string]string, exporter.FinalizeFunc, exporter.DescriptorReference, error) {
	if len(src.Refs) > 0 {
		return nil, nil, nil, fmt.Errorf("exporting a multi-platform build to the engine's images is not supported")
	}
	var cfg v1.ConfigFile
	if raw, ok := src.Metadata[exptypes.ExporterImageConfigKey]; ok {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, nil, nil, fmt.Errorf("image config: %w", err)
		}
	}
	if cfg.Created.IsZero() {
		cfg.Created = v1.Time{Time: time.Now().UTC()}
	}
	if err := os.MkdirAll(m.store.BuildDir(), 0o700); err != nil {
		return nil, nil, nil, err
	}
	dst, err := os.MkdirTemp(m.store.BuildDir(), "export-")
	if err != nil {
		return nil, nil, nil, err
	}
	// MkdirTemp creates the directory; the clone needs it absent.
	os.Remove(dst)
	root := dst
	if src.Ref == nil {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, nil, nil, err
		}
	} else {
		mountable, err := src.Ref.Mount(ctx, true, session.NewGroup(info.SessionID))
		if err != nil {
			return nil, nil, nil, err
		}
		ms, release, err := mountable.Mount()
		if err != nil {
			return nil, nil, nil, err
		}
		dir, done, err := bindDir(ms)
		if err == nil {
			err = image.CloneDir(dir, root)
			done()
		}
		release()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	img, err := m.store.AddRoot(root, cfg, m.names)
	if err != nil {
		os.RemoveAll(root)
		return nil, nil, nil, err
	}
	return map[string]string{
		exptypes.ExporterImageDigestKey:       img.ID,
		exptypes.ExporterImageConfigDigestKey: img.ID,
		exptypes.ExporterImageNameKey:         strings.Join(m.names, ","),
	}, nil, nil, nil
}
