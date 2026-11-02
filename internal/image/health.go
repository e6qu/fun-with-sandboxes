package image

import (
	"bytes"
	"encoding/json"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
)

// Config is an image config as the engine writes one: go-containerregistry's
// config, which has no field for the health check's start interval, and
// that interval.
type Config struct {
	v1.ConfigFile
	HealthStartInterval time.Duration
}

// healthStartInterval reads the health check's start interval from a raw
// image config.
func healthStartInterval(raw []byte) time.Duration {
	var c struct {
		Config struct {
			Healthcheck *struct{ StartInterval time.Duration }
		} `json:"config"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Config.Healthcheck == nil {
		return 0
	}
	return c.Config.Healthcheck.StartInterval
}

// withStartInterval returns img with its config's health check carrying
// the start interval d.
func withStartInterval(img v1.Image, d time.Duration) (v1.Image, error) {
	raw, err := img.RawConfigFile()
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	cfg, _ := doc["config"].(map[string]any)
	if cfg == nil {
		return img, nil
	}
	hc, _ := cfg["Healthcheck"].(map[string]any)
	if hc == nil {
		return img, nil
	}
	hc["StartInterval"] = int64(d)
	patched, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	m, err := img.Manifest()
	if err != nil {
		return nil, err
	}
	m = m.DeepCopy()
	h, size, err := v1.SHA256(bytes.NewReader(patched))
	if err != nil {
		return nil, err
	}
	m.Config.Digest, m.Config.Size = h, size
	rawManifest, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return &patchedConfig{Image: img, raw: patched, manifest: m, rawManifest: rawManifest}, nil
}

// patchedConfig is an image whose config is raw, and its manifest the one
// that names it.
type patchedConfig struct {
	v1.Image
	raw         []byte
	manifest    *v1.Manifest
	rawManifest []byte
}

func (p *patchedConfig) RawConfigFile() ([]byte, error) { return p.raw, nil }

func (p *patchedConfig) ConfigFile() (*v1.ConfigFile, error) {
	return v1.ParseConfigFile(bytes.NewReader(p.raw))
}

func (p *patchedConfig) ConfigName() (v1.Hash, error) { return p.manifest.Config.Digest, nil }

func (p *patchedConfig) Manifest() (*v1.Manifest, error) { return p.manifest.DeepCopy(), nil }

func (p *patchedConfig) RawManifest() ([]byte, error) { return p.rawManifest, nil }

func (p *patchedConfig) Digest() (v1.Hash, error) { return partial.Digest(p) }

func (p *patchedConfig) Size() (int64, error) { return partial.Size(p) }
