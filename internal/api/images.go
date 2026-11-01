package api

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/moby/moby/api/types/registry"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// jsonMessage is one line of the progress stream Docker clients read from
// pulls and loads.
type jsonMessage struct {
	Status      string        `json:"status,omitempty"`
	Aux         any           `json:"aux,omitempty"`
	ID          string        `json:"id,omitempty"`
	Stream      string        `json:"stream,omitempty"`
	Error       string        `json:"error,omitempty"`
	ErrorDetail *errorDetails `json:"errorDetail,omitempty"`
}

type errorDetails struct {
	Message string `json:"message"`
}

func (s *Server) imagePull(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("fromSrc") != "" {
		writeError(w, http.StatusNotImplemented, "importing images from a tarball is not supported; use docker load")
		return
	}
	ref := q.Get("fromImage")
	if tag := q.Get("tag"); tag != "" {
		if strings.HasPrefix(tag, "sha256:") {
			ref += "@" + tag
		} else {
			ref += ":" + tag
		}
	}
	if p := q.Get("platform"); p != "" && p != image.Platform.OS+"/"+image.Platform.Architecture {
		writeError(w, http.StatusNotImplemented, "requested platform "+p+" does not match the engine platform "+image.Platform.OS+"/"+image.Platform.Architecture)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	tag := q.Get("tag")
	if tag == "" {
		tag = "latest"
	}
	repo := q.Get("fromImage")
	if r, err := name.ParseReference(ref); err == nil {
		repo = r.Context().RepositoryStr()
	}
	enc.Encode(jsonMessage{Status: "Pulling from " + repo, ID: tag})
	flush(w)
	pulled, err := s.e.Images().Pull(r.Context(), ref, func(id, status string) {
		enc.Encode(jsonMessage{Status: status, ID: id})
		flush(w)
	})
	if err != nil {
		enc.Encode(jsonMessage{Error: err.Error(), ErrorDetail: &errorDetails{Message: err.Error()}})
		return
	}
	enc.Encode(jsonMessage{Status: "Digest: " + pulled.Digest})
	if pulled.UpToDate {
		enc.Encode(jsonMessage{Status: "Status: Image is up to date for " + pulled.Ref})
	} else {
		enc.Encode(jsonMessage{Status: "Status: Downloaded newer image for " + pulled.Ref})
	}
}

func (s *Server) imageLoad(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.e.Images().Load(r.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	if err != nil {
		enc.Encode(jsonMessage{Error: err.Error(), ErrorDetail: &errorDetails{Message: err.Error()}})
		return
	}
	for _, img := range imgs {
		if len(img.RepoTags) == 0 {
			enc.Encode(jsonMessage{Stream: "Loaded image ID: " + img.ID + "\n"})
		}
		for _, t := range img.RepoTags {
			enc.Encode(jsonMessage{Stream: "Loaded image: " + t + "\n"})
		}
	}
}

// imageGet serves GET /images/{ref}/json; image references contain slashes,
// so the route captures the rest of the path.
func (s *Server) imageGet(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	switch {
	case strings.HasSuffix(ref, "/json"):
		res, err := s.e.ImageInspect(strings.TrimSuffix(ref, "/json"))
		respond(w, res, err)
	case strings.HasSuffix(ref, "/history"):
		res, err := s.e.History(strings.TrimSuffix(ref, "/history"))
		respond(w, res, err)
	case ref == "get":
		s.save(w, r.URL.Query()["names"])
	case strings.HasSuffix(ref, "/get"):
		s.save(w, []string{strings.TrimSuffix(ref, "/get")})
	default:
		writeError(w, http.StatusNotFound, "page not found")
	}
}

func (s *Server) imagePost(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	switch {
	case strings.HasSuffix(ref, "/tag"):
		q := r.URL.Query()
		respondStatus(w, http.StatusCreated, s.e.ImageTag(strings.TrimSuffix(ref, "/tag"), q.Get("repo"), q.Get("tag")))
	case ref == "prune":
		f, err := engine.ParseFilters(r.URL.Query().Get("filters"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		res, err := s.e.ImagePrune(f)
		respond(w, res, err)
	case strings.HasSuffix(ref, "/push"):
		s.push(w, r, strings.TrimSuffix(ref, "/push"))
	default:
		writeError(w, http.StatusNotFound, "page not found")
	}
}

func (s *Server) build(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("version") == "2" {
		writeError(w, http.StatusBadRequest, "POST /build is the classic builder; BuildKit builds run through POST /grpc, as docker buildx does")
		return
	}
	if q.Get("remote") != "" {
		writeError(w, http.StatusNotImplemented, "building from a remote context is not supported")
		return
	}
	if p := q.Get("platform"); p != "" && p != image.Platform.OS+"/"+image.Platform.Architecture {
		writeError(w, http.StatusNotImplemented, "requested platform "+p+" does not match the engine platform "+image.Platform.OS+"/"+image.Platform.Architecture)
		return
	}
	opts := engine.BuildOptions{Dockerfile: q.Get("dockerfile"), Tags: q["t"], Target: q.Get("target"), NetworkMode: q.Get("networkmode")}
	for _, p := range []struct {
		key string
		dst any
	}{{"buildargs", &opts.BuildArgs}, {"labels", &opts.Labels}} {
		if v := q.Get(p.key); v != "" {
			if err := json.Unmarshal([]byte(v), p.dst); err != nil {
				writeError(w, http.StatusBadRequest, "invalid "+p.key+": "+err.Error())
				return
			}
		}
	}
	quiet := q.Get("q") == "1" || q.Get("q") == "true"
	// Progress streams while the context upload is still being read; an
	// HTTP/1 server closes the request body once a response starts unless
	// told otherwise.
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flush(w)
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	send := func(m jsonMessage) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(m)
		flush(w)
	}
	img, err := s.e.Build(r.Context(), opts, r.Body, func(line string) {
		if !quiet {
			send(jsonMessage{Stream: line})
		}
	})
	if err != nil {
		send(jsonMessage{Error: err.Error(), ErrorDetail: &errorDetails{Message: err.Error()}})
		return
	}
	send(jsonMessage{Aux: map[string]string{"ID": img.ID}})
	if quiet {
		send(jsonMessage{Stream: img.ID + "\n"})
	}
}

// save streams a `docker save` archive. Errors before the first byte get a
// status; once the archive streams, a failure can only cut it short.
func (s *Server) save(w http.ResponseWriter, refs []string) {
	if len(refs) == 0 {
		writeError(w, http.StatusBadRequest, "no images named")
		return
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(s.e.Save(refs, pw)) }()
	buf := bufio.NewReader(pr)
	if _, err := buf.Peek(1); err != nil && err != io.EOF {
		pr.Close()
		writeEngineError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	io.Copy(w, buf)
}

// registryAuth decodes the X-Registry-Auth header the Docker CLI sends with
// pushes: base64url-encoded JSON credentials.
func registryAuth(header string) (authn.Authenticator, error) {
	if header == "" {
		return authn.Anonymous, nil
	}
	raw, err := base64.URLEncoding.DecodeString(header)
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(header); err != nil {
			return nil, fmt.Errorf("X-Registry-Auth: %w", err)
		}
	}
	var ac registry.AuthConfig
	if err := json.Unmarshal(raw, &ac); err != nil {
		return nil, fmt.Errorf("X-Registry-Auth: %w", err)
	}
	cfg := authn.AuthConfig{Username: ac.Username, Password: ac.Password, Auth: ac.Auth, IdentityToken: ac.IdentityToken, RegistryToken: ac.RegistryToken}
	if cfg == (authn.AuthConfig{}) {
		return authn.Anonymous, nil
	}
	return authn.FromConfig(cfg), nil
}

func (s *Server) push(w http.ResponseWriter, r *http.Request, repo string) {
	auth, err := registryAuth(r.Header.Get("X-Registry-Auth"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	results, err := s.e.Push(r.Context(), repo, r.URL.Query().Get("tag"), auth, func(line string) {
		enc.Encode(jsonMessage{Status: line})
		flush(w)
	})
	for _, res := range results {
		enc.Encode(jsonMessage{Aux: map[string]any{"Tag": res.Tag, "Digest": res.Digest, "Size": res.Size}})
	}
	if err != nil {
		enc.Encode(jsonMessage{Error: err.Error(), ErrorDetail: &errorDetails{Message: err.Error()}})
	}
}
