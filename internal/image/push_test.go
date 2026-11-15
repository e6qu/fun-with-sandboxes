package image

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// requestLog counts a registry's requests by method and kind.
type requestLog struct {
	mu sync.Mutex
	n  map[string]int
}

func (r *requestLog) count(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[key]
}

func (r *requestLog) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n = map[string]int{}
}

// countingRegistry is a registry that counts its requests and, unlike
// go-containerregistry's, which keeps blobs for all its repositories
// together, holds a blob in a repository only once it was sent or mounted
// there.
func countingRegistry(t *testing.T) (string, *requestLog) {
	t.Helper()
	l := &requestLog{n: map[string]int{}}
	held := map[string]bool{}
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		repo, _, _ := strings.Cut(strings.TrimPrefix(req.URL.Path, "/v2/"), "/blobs/")
		key := req.Method + " other"
		switch {
		case strings.Contains(req.URL.Path, "/blobs/uploads"):
			key = req.Method + " upload"
			if d := req.URL.Query().Get("digest"); d != "" {
				l.mu.Lock()
				held[repo+"@"+d] = true
				l.mu.Unlock()
			}
			if d := req.URL.Query().Get("mount"); d != "" && req.URL.Query().Get("from") != "" {
				key = "mount"
				l.mu.Lock()
				ok := held[req.URL.Query().Get("from")+"@"+d]
				if ok {
					held[repo+"@"+d] = true
				}
				l.mu.Unlock()
				if ok {
					l.mu.Lock()
					l.n[key]++
					l.mu.Unlock()
					w.Header().Set("Location", "/v2/"+repo+"/blobs/"+d)
					w.Header().Set("Docker-Content-Digest", d)
					w.WriteHeader(http.StatusCreated)
					return
				}
				q := req.URL.Query()
				q.Del("mount")
				q.Del("from")
				req.URL.RawQuery = q.Encode()
			}
		case strings.Contains(req.URL.Path, "/blobs/"):
			key = req.Method + " blob"
			l.mu.Lock()
			ok := held[repo+"@"+req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]]
			l.n[key]++
			l.mu.Unlock()
			if req.Method == http.MethodHead && !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			reg.ServeHTTP(w, req)
			return
		case strings.Contains(req.URL.Path, "/manifests/"):
			key = req.Method + " manifest"
		}
		l.mu.Lock()
		l.n[key]++
		l.mu.Unlock()
		reg.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), l
}

func pushAll(t *testing.T, s *Store, ref string, progress Progress) {
	t.Helper()
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPusher(context.Background(), authn.Anonymous)
	if err != nil {
		t.Fatal(err)
	}
	img, rec, err := s.V1Image(ref)
	if err != nil {
		t.Fatal(err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	sources := s.LayerSources(tag.Context())
	for _, l := range layers {
		dg, _ := l.Digest()
		var from *name.Repository
		if r, ok := sources[dg]; ok {
			from = &r
		}
		if err := p.PushLayer(tag.Context(), l, from, progress); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.PushManifest(tag, img); err != nil {
		t.Fatal(err)
	}
	dg, _ := img.Digest()
	if err := s.AddReference(rec.ID, Familiar(tag.Context())+"@"+dg.String()); err != nil {
		t.Fatal(err)
	}
}

// A push asks once whether the registry holds each blob, and mounts the
// layers another repository of the registry holds rather than sending
// them.
func TestPushChecksOnceAndMounts(t *testing.T) {
	host, reqs := countingRegistry(t)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	img := darwinImage(t, 2)
	if _, err := s.Add(img, []string{host + "/r/base:1"}, ""); err != nil {
		t.Fatal(err)
	}
	var log progressLog
	pushAll(t, s, host+"/r/base:1", log.record)
	// Two layers and the config: one HEAD each, then each sent.
	if n := reqs.count("HEAD blob"); n != 3 {
		t.Errorf("%d HEAD requests for 3 blobs", n)
	}
	if log.count("Pushed") != 2 || log.count("Pushing") != 2 {
		t.Errorf("progress %v", log)
	}
	if _, err := remote.Image(mustRef(t, host+"/r/base:1")); err != nil {
		t.Fatalf("the pushed image does not read back: %v", err)
	}

	reqs.reset()
	log = nil
	if err := s.Tag(host+"/r/base:1", host+"/r/app:1"); err != nil {
		t.Fatal(err)
	}
	pushAll(t, s, host+"/r/app:1", log.record)
	if n := reqs.count("mount"); n != 2 {
		t.Errorf("%d mounts, want the two layers'", n)
	}
	if n := reqs.count("PUT upload"); n != 1 {
		t.Errorf("%d uploads, want the config's alone", n)
	}
	if log.count("Mounted from r/base") != 2 {
		t.Errorf("progress %v", log)
	}
	if _, err := remote.Image(mustRef(t, host+"/r/app:1")); err != nil {
		t.Fatalf("the pushed image does not read back: %v", err)
	}

	reqs.reset()
	log = nil
	pushAll(t, s, host+"/r/app:1", log.record)
	if n := reqs.count("HEAD blob"); n != 3 || log.count("Layer already exists") != 2 {
		t.Errorf("pushing again: %d HEAD requests, progress %v", n, log)
	}
}

func mustRef(t *testing.T, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var _ v1.Layer = (*readLayer)(nil)
