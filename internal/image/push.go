package image

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Pusher sends images' blobs and manifests to a registry over one
// authenticated session, checking each blob's presence once: a blob it
// sent or found once it does not ask about again.
type Pusher struct {
	opts  []remote.Option
	heads *headLog
	mu    sync.Mutex
	done  map[v1.Hash]bool
}

// NewPusher starts a push with the client's credentials.
func NewPusher(ctx context.Context, auth authn.Authenticator) (*Pusher, error) {
	heads := &headLog{rt: remote.DefaultTransport, found: map[string]bool{}}
	p, err := remote.NewPusher(remote.WithAuth(auth), remote.WithContext(ctx), remote.WithTransport(heads))
	if err != nil {
		return nil, err
	}
	return &Pusher{opts: []remote.Option{remote.Reuse(p), remote.WithContext(ctx)}, heads: heads, done: map[v1.Hash]bool{}}, nil
}

// PushLayer sends l to repo unless repo holds it, mounting it from from,
// a repository of the same registry that holds it, where from is set. It
// reports as dockerd does: Preparing, then Layer already exists, Mounted
// from a repository, or Pushing and Pushed.
func (p *Pusher) PushLayer(repo name.Repository, l v1.Layer, from *name.Repository, progress Progress) error {
	dg, err := l.Digest()
	if err != nil {
		return err
	}
	id := dg.Hex[:12]
	progress(id, "Preparing")
	p.mu.Lock()
	done := p.done[dg]
	p.mu.Unlock()
	if done {
		progress(id, "Layer already exists")
		return nil
	}
	read := &readLayer{Layer: l, first: func() { progress(id, "Pushing") }}
	var send v1.Layer = read
	if from != nil {
		send = &remote.MountableLayer{Layer: read, Reference: from.Digest(dg.String())}
	}
	if err := remote.WriteLayer(repo, send, p.opts...); err != nil {
		return err
	}
	p.mu.Lock()
	p.done[dg] = true
	p.mu.Unlock()
	switch {
	case read.read():
		progress(id, "Pushed")
	case from != nil && !p.heads.has(dg):
		progress(id, "Mounted from "+from.RepositoryStr())
	default:
		progress(id, "Layer already exists")
	}
	return nil
}

// PushManifest sends img's config, unless the repository holds it, and
// then its manifest under ref. The layers must be there already.
func (p *Pusher) PushManifest(ref name.Reference, img v1.Image) error {
	cl, err := partial.ConfigLayer(img)
	if err != nil {
		return err
	}
	if err := remote.WriteLayer(ref.Context(), cl, p.opts...); err != nil {
		return err
	}
	return remote.Put(ref, img, p.opts...)
}

// readLayer is a layer that reports the first time its content is read,
// which only an upload does.
type readLayer struct {
	v1.Layer
	once  sync.Once
	first func()
	mu    sync.Mutex
	was   bool
}

func (r *readLayer) Compressed() (io.ReadCloser, error) {
	r.once.Do(r.first)
	r.mu.Lock()
	r.was = true
	r.mu.Unlock()
	return r.Layer.Compressed()
}

func (r *readLayer) read() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.was
}

// headLog notes the blobs a registry answered a HEAD request for as held.
type headLog struct {
	rt    http.RoundTripper
	mu    sync.Mutex
	found map[string]bool
}

func (h *headLog) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := h.rt.RoundTrip(req)
	if err == nil && req.Method == http.MethodHead && res.StatusCode == http.StatusOK && strings.Contains(req.URL.Path, "/blobs/") {
		h.mu.Lock()
		h.found[req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]] = true
		h.mu.Unlock()
	}
	return res, err
}

func (h *headLog) has(dg v1.Hash) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.found[dg.String()]
}

// LayerSources maps each layer of the stored images that a repository of
// repo's registry other than repo holds, as a pull from it or a push to it
// recorded, to that repository, from which a push to repo can mount it.
func (s *Store) LayerSources(repo name.Repository) map[v1.Hash]name.Repository {
	out := map[v1.Hash]name.Repository{}
	for _, img := range s.List() {
		var from []name.Repository
		for _, d := range img.RepoDigests {
			r, err := name.NewDigest(d)
			if err != nil || r.Context().RegistryStr() != repo.RegistryStr() || r.Context().RepositoryStr() == repo.RepositoryStr() {
				continue
			}
			from = append(from, r.Context())
		}
		if len(from) == 0 {
			continue
		}
		v, _, err := s.V1Image(img.ID)
		if err != nil {
			continue
		}
		m, err := v.Manifest()
		if err != nil {
			continue
		}
		for _, l := range m.Layers {
			if _, ok := out[l.Digest]; !ok {
				out[l.Digest] = from[0]
			}
		}
	}
	return out
}
