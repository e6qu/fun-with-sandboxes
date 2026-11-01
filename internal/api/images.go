package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

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
	enc.Encode(jsonMessage{Status: "Pulling from " + q.Get("fromImage"), ID: tag})
	flush(w)
	img, err := s.e.Images().Pull(ref)
	if err != nil {
		enc.Encode(jsonMessage{Error: err.Error(), ErrorDetail: &errorDetails{Message: err.Error()}})
		return
	}
	enc.Encode(jsonMessage{Status: "Digest: " + img.ID})
	enc.Encode(jsonMessage{Status: "Status: Downloaded newer image for " + ref})
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
	case strings.HasSuffix(ref, "/history"), strings.HasSuffix(ref, "/get"):
		writeError(w, http.StatusNotImplemented, "image history and export are not supported by this engine")
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
	case strings.HasSuffix(ref, "/push"):
		writeError(w, http.StatusNotImplemented, "pushing images is not supported by this engine")
	default:
		writeError(w, http.StatusNotFound, "page not found")
	}
}

func (s *Server) build(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("version") == "2" {
		writeError(w, http.StatusNotImplemented, "BuildKit is not supported by this engine; use the classic builder (DOCKER_BUILDKIT=0)")
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
