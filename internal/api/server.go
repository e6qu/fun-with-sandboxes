// Package api serves the Docker Engine API subset that the Docker CLI,
// Docker Compose and Testcontainers use, backed by the engine.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"golang.org/x/net/websocket"

	"github.com/e6qu/fun-with-sandboxes/internal/cri"
	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// BuildKit serves BuildKit's control API and sessions.
type BuildKit interface {
	ServeGRPC(http.ResponseWriter, *http.Request)
	ServeSession(http.ResponseWriter, *http.Request)
	Prune(ctx context.Context, all bool) ([]string, uint64, error)
	DiskUsage(ctx context.Context) ([]build.CacheRecord, error)
}

type Server struct {
	e   *engine.Engine
	bk  BuildKit
	mux *http.ServeMux
	// lp serves the libpod endpoints that differ from Docker's.
	lp      *http.ServeMux
	pods    *podStore
	started time.Time
	cri     *cri.Service
}

// New serves the engine's API; bk, when set, also serves BuildKit.
func New(e *engine.Engine, bk BuildKit) *Server {
	s := &Server{e: e, bk: bk, mux: http.NewServeMux(), lp: http.NewServeMux(), pods: openPods(e.Root()), started: time.Now(),
		cri: cri.New(e, cri.DefaultSandboxImage)}
	s.routes()
	s.libpodRoutes()
	return s
}

// Close stops what the server runs beside its handlers: the CRI's stream
// server and its log copies.
func (s *Server) Close() { s.cri.Close() }

var versionPrefix = regexp.MustCompile(`^/v(\d+\.\d+)(/.*)$`)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The socket serves the Container Runtime Interface too, as gRPC over
	// HTTP/2, where the kubelet's --container-runtime-endpoint names it.
	if cri.Serves(r) {
		s.cri.ServeHTTP(w, r)
		return
	}
	if p, ok := libpodPath(r.URL.Path); ok {
		s.serveLibpod(w, r, p)
		return
	}
	if m := versionPrefix.FindStringSubmatch(r.URL.Path); m != nil {
		if cmpVersion(m[1], engine.APIVersion) > 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("client version %s is too new. Maximum supported API version is %s", m[1], engine.APIVersion))
			return
		}
		if cmpVersion(m[1], engine.MinAPIVersion) < 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("client version %s is too old. Minimum supported API version is %s, please upgrade your client to a newer version", m[1], engine.MinAPIVersion))
			return
		}
		r.URL.Path = m[2]
		r.URL.RawPath = ""
		r = r.WithContext(context.WithValue(r.Context(), versionKey{}, m[1]))
	}
	linkReference(r.URL)
	w.Header().Set("Api-Version", engine.APIVersion)
	w.Header().Set("Server", "fun-with-sandboxes/"+engine.Version+" (darwin)")
	w.Header().Set("Docker-Experimental", "false")
	w.Header().Set("Ostype", "darwin")
	s.mux.ServeHTTP(w, r)
}

type versionKey struct{}

// apiVersion is the API version the request's path names, the engine's
// own when it names none.
func apiVersion(r *http.Request) string {
	if v, ok := r.Context().Value(versionKey{}).(string); ok {
		return v
	}
	return engine.APIVersion
}

func cmpVersion(a, b string) int {
	pa, pb := strings.SplitN(a, ".", 2), strings.SplitN(b, ".", 2)
	for i := 0; i < 2; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// containerActions are the routes under a container's reference.
var containerActions = []string{"/attach/ws", "/json", "/start", "/stop", "/restart", "/kill", "/pause", "/unpause", "/wait",
	"/rename", "/resize", "/update", "/attach", "/exec", "/logs", "/stats", "/top", "/changes", "/export", "/archive"}

// linkReference escapes the slashes of a container reference that names a
// link, /containers/<parent>/<alias>/json, which Docker's router takes
// whole, so that the route's {id} holds it.
func linkReference(u *url.URL) {
	rest, ok := strings.CutPrefix(u.Path, "/containers/")
	if !ok {
		return
	}
	for _, a := range containerActions {
		ref, ok := strings.CutSuffix(rest, a)
		if ok && strings.Contains(ref, "/") {
			u.RawPath = "/containers/" + url.PathEscape(ref) + a
			return
		}
	}
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /_ping", s.ping)
	m.HandleFunc("HEAD /_ping", s.ping)
	m.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.e.Version()) })
	m.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.e.Info()) })
	m.HandleFunc("GET /events", s.events)
	m.HandleFunc("GET /system/df", s.systemDF)
	m.HandleFunc("GET /distribution/{ref...}", s.distribution)

	m.HandleFunc("GET /containers/json", s.containerList)
	m.HandleFunc("POST /containers/create", s.containerCreate)
	m.HandleFunc("POST /containers/prune", s.containerPrune)
	m.HandleFunc("GET /containers/{id}/json", s.containerInspect)
	m.HandleFunc("GET /containers/{id}/changes", func(w http.ResponseWriter, r *http.Request) {
		res, err := s.e.Changes(r.Context(), r.PathValue("id"))
		respond(w, res, err)
	})
	m.HandleFunc("POST /containers/{id}/start", s.simple(func(r *http.Request) error { return s.e.Start(r.PathValue("id")) }, http.StatusNoContent))
	m.HandleFunc("POST /containers/{id}/stop", s.simple(func(r *http.Request) error {
		return s.e.Stop(r.PathValue("id"), timeoutParam(r), r.URL.Query().Get("signal"))
	}, http.StatusNoContent))
	m.HandleFunc("POST /containers/{id}/restart", s.simple(func(r *http.Request) error {
		return s.e.Restart(r.PathValue("id"), timeoutParam(r), r.URL.Query().Get("signal"))
	}, http.StatusNoContent))
	m.HandleFunc("POST /containers/{id}/kill", s.simple(func(r *http.Request) error {
		return s.e.Kill(r.PathValue("id"), r.URL.Query().Get("signal"))
	}, http.StatusNoContent))
	m.HandleFunc("POST /containers/{id}/rename", s.simple(func(r *http.Request) error {
		return s.e.Rename(r.PathValue("id"), r.URL.Query().Get("name"))
	}, http.StatusNoContent))
	m.HandleFunc("POST /containers/{id}/resize", s.simple(func(r *http.Request) error {
		h, w := sizeParams(r)
		return s.e.Resize(r.PathValue("id"), h, w)
	}, http.StatusOK))
	m.HandleFunc("POST /containers/{id}/wait", s.containerWait)
	m.HandleFunc("GET /containers/{id}/logs", s.containerLogs)
	m.HandleFunc("POST /containers/{id}/attach", s.containerAttach)
	m.HandleFunc("GET /containers/{id}/attach/ws", s.containerAttachWS)
	m.HandleFunc("DELETE /containers/{id...}", s.simple(func(r *http.Request) error {
		q := r.URL.Query()
		if boolParam(q.Get("link")) {
			return s.e.RemoveLink(r.PathValue("id"))
		}
		return s.e.Remove(r.PathValue("id"), engine.RemoveOptions{Force: boolParam(q.Get("force")), RemoveVolumes: boolParam(q.Get("v"))})
	}, http.StatusNoContent))
	m.HandleFunc("GET /containers/{id}/top", func(w http.ResponseWriter, r *http.Request) {
		res, err := s.e.Top(r.PathValue("id"), r.URL.Query().Get("ps_args"))
		respond(w, res, err)
	})
	m.HandleFunc("GET /containers/{id}/stats", s.containerStats)
	m.HandleFunc("POST /containers/{id}/pause", s.simple(func(r *http.Request) error { return s.e.Pause(r.PathValue("id")) }, http.StatusNoContent))
	m.HandleFunc("POST /containers/{id}/unpause", s.simple(func(r *http.Request) error { return s.e.Unpause(r.PathValue("id")) }, http.StatusNoContent))
	m.HandleFunc("POST /containers/{id}/update", func(w http.ResponseWriter, r *http.Request) {
		var req container.UpdateConfig
		if !decode(w, r, &req) {
			return
		}
		res, err := s.e.Update(r.PathValue("id"), req)
		respond(w, res, err)
	})
	m.HandleFunc("GET /containers/{id}/export", func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.e.Container(r.PathValue("id")); err != nil {
			writeEngineError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		s.e.Export(r.PathValue("id"), w)
	})
	m.HandleFunc("HEAD /containers/{id}/archive", s.archiveHead)
	m.HandleFunc("GET /containers/{id}/archive", s.archiveGet)
	m.HandleFunc("PUT /containers/{id}/archive", s.archivePut)
	m.HandleFunc("POST /containers/{id}/exec", s.execCreate)
	m.HandleFunc("POST /exec/{id}/start", s.execStart)
	m.HandleFunc("POST /exec/{id}/resize", s.simple(func(r *http.Request) error {
		h, w := sizeParams(r)
		return s.e.ExecResize(r.PathValue("id"), h, w)
	}, http.StatusOK))
	m.HandleFunc("GET /exec/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		res, err := s.e.ExecInspect(r.PathValue("id"))
		respond(w, res, err)
	})

	m.HandleFunc("GET /images/json", func(w http.ResponseWriter, r *http.Request) {
		f, err := engine.ParseFilters(r.URL.Query().Get("filters"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.e.ImageList(f))
	})
	m.HandleFunc("POST /auth", s.login)
	m.HandleFunc("POST /images/create", s.imagePull)
	m.HandleFunc("POST /images/load", s.imageLoad)
	m.HandleFunc("GET /images/{ref...}", s.imageGet)
	m.HandleFunc("POST /images/{ref...}", s.imagePost)
	m.HandleFunc("DELETE /images/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		res, err := s.e.ImageRemove(r.PathValue("ref"), boolParam(r.URL.Query().Get("force")))
		respond(w, res, err)
	})

	m.HandleFunc("GET /networks", func(w http.ResponseWriter, r *http.Request) {
		f, err := engine.ParseFilters(r.URL.Query().Get("filters"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.e.NetworkList(f))
	})
	m.HandleFunc("GET /networks/{id}", func(w http.ResponseWriter, r *http.Request) {
		res, err := s.e.NetworkInspect(r.PathValue("id"))
		respond(w, res, err)
	})
	m.HandleFunc("POST /networks/create", s.networkCreate)
	m.HandleFunc("POST /networks/prune", func(w http.ResponseWriter, r *http.Request) {
		f, err := engine.ParseFilters(r.URL.Query().Get("filters"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		removed, err := s.e.NetworkPrune(f)
		if removed == nil {
			removed = []string{}
		}
		respond(w, network.PruneReport{NetworksDeleted: removed}, err)
	})
	m.HandleFunc("POST /networks/{id}/connect", func(w http.ResponseWriter, r *http.Request) {
		var req network.ConnectRequest
		if !decode(w, r, &req) {
			return
		}
		respondStatus(w, http.StatusOK, s.e.NetworkConnect(r.PathValue("id"), req))
	})
	m.HandleFunc("POST /networks/{id}/disconnect", func(w http.ResponseWriter, r *http.Request) {
		var req network.DisconnectRequest
		if !decode(w, r, &req) {
			return
		}
		respondStatus(w, http.StatusOK, s.e.NetworkDisconnect(r.PathValue("id"), req.Container, req.Force))
	})
	m.HandleFunc("DELETE /networks/{id}", s.simple(func(r *http.Request) error { return s.e.NetworkRemove(r.PathValue("id")) }, http.StatusNoContent))

	m.HandleFunc("GET /volumes", func(w http.ResponseWriter, r *http.Request) {
		f, err := engine.ParseFilters(r.URL.Query().Get("filters"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		vols := s.e.VolumeList(f)
		list := make([]volume.Volume, len(vols))
		copy(list, vols)
		writeJSON(w, http.StatusOK, volume.ListResponse{Volumes: list, Warnings: []string{}})
	})
	m.HandleFunc("POST /volumes/create", func(w http.ResponseWriter, r *http.Request) {
		var req volume.CreateRequest
		if !decode(w, r, &req) {
			return
		}
		v, err := s.e.VolumeCreate(req)
		respondCode(w, http.StatusCreated, v, err)
	})
	m.HandleFunc("POST /volumes/prune", func(w http.ResponseWriter, r *http.Request) {
		f, err := engine.ParseFilters(r.URL.Query().Get("filters"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		rep, err := s.e.VolumePrune(f)
		if rep.VolumesDeleted == nil {
			rep.VolumesDeleted = []string{}
		}
		respond(w, rep, err)
	})
	m.HandleFunc("GET /volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.e.VolumeInspect(r.PathValue("name"))
		respond(w, v, err)
	})
	m.HandleFunc("DELETE /volumes/{name}", s.simple(func(r *http.Request) error {
		return s.e.VolumeRemove(r.PathValue("name"), boolParam(r.URL.Query().Get("force")))
	}, http.StatusNoContent))

	unsupported := func(what string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotImplemented, what+" is not supported by this engine")
		}
	}
	m.HandleFunc("POST /build", s.build)
	m.HandleFunc("POST /build/prune", s.buildPrune)
	if s.bk != nil {
		m.HandleFunc("POST /grpc", s.bk.ServeGRPC)
		m.HandleFunc("POST /session", s.bk.ServeSession)
	} else {
		m.HandleFunc("POST /grpc", unsupported("BuildKit"))
		m.HandleFunc("POST /session", unsupported("BuildKit sessions"))
	}
	m.HandleFunc("POST /commit", s.commit)
}

func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	// "2" sends the Docker CLI and buildx to BuildKit over /grpc and
	// /session; "1" to the classic builder over POST /build.
	if s.bk != nil {
		w.Header().Set("Builder-Version", "2")
	} else {
		w.Header().Set("Builder-Version", "1")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		io.WriteString(w, "OK")
	}
}

func (s *Server) simple(fn func(*http.Request) error, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		respondStatus(w, status, fn(r))
	}
}

func boolParam(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func timeoutParam(r *http.Request) *int {
	v := r.URL.Query().Get("t")
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

func sizeParams(r *http.Request) (uint16, uint16) {
	h, _ := strconv.Atoi(r.URL.Query().Get("h"))
	w, _ := strconv.Atoi(r.URL.Query().Get("w"))
	return uint16(h), uint16(w)
}

func (s *Server) containerList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := engine.ParseFilters(q.Get("filters"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	writeJSON(w, http.StatusOK, s.e.List(engine.ListOptions{All: boolParam(q.Get("all")), Limit: limit, Filters: f, Size: boolParam(q.Get("size"))}))
}

func (s *Server) containerCreate(w http.ResponseWriter, r *http.Request) {
	var req container.CreateRequest
	if !decode(w, r, &req) {
		return
	}
	q := r.URL.Query()
	res, err := s.e.Create(engine.CreateOptions{Name: q.Get("name"), Platform: q.Get("platform")}, req)
	respondCode(w, http.StatusCreated, res, err)
}

func (s *Server) containerPrune(w http.ResponseWriter, r *http.Request) {
	f, err := engine.ParseFilters(r.URL.Query().Get("filters"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	removed, size, err := s.e.Prune(f)
	if removed == nil {
		removed = []string{}
	}
	respond(w, container.PruneReport{ContainersDeleted: removed, SpaceReclaimed: size}, err)
}

func (s *Server) containerInspect(w http.ResponseWriter, r *http.Request) {
	res, err := s.e.Inspect(r.PathValue("id"))
	respond(w, res, err)
}

func (s *Server) containerWait(w http.ResponseWriter, r *http.Request) {
	cond := container.WaitCondition(r.URL.Query().Get("condition"))
	switch cond {
	case "", container.WaitConditionNotRunning, container.WaitConditionNextExit, container.WaitConditionRemoved:
	default:
		writeError(w, http.StatusBadRequest, "invalid condition: "+string(cond))
		return
	}
	ch, err := s.e.Wait(r.PathValue("id"), cond)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	// Send the headers at once: clients start the container only after the
	// wait is in place.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flush(w)
	select {
	case res := <-ch:
		json.NewEncoder(w).Encode(res)
	case <-r.Context().Done():
	}
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) containerLogs(w http.ResponseWriter, r *http.Request) {
	s.logs(w, r, false)
}

// logs serves a container's logs, raw for a TTY container as Docker sends
// them, or always framed by stream, as Podman's API sends them.
func (s *Server) logs(w http.ResponseWriter, r *http.Request, framed bool) {
	q := r.URL.Query()
	id := r.PathValue("id")
	c, err := s.e.Container(id)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	opts := engine.LogOptions{Stdout: boolParam(q.Get("stdout")), Stderr: boolParam(q.Get("stderr")), Follow: boolParam(q.Get("follow")), Tail: -1}
	if !opts.Stdout && !opts.Stderr {
		writeError(w, http.StatusBadRequest, "Bad parameters: you must choose at least one stream")
		return
	}
	if t := q.Get("tail"); t != "" && t != "all" {
		n, err := strconv.Atoi(t)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid tail value: "+t)
			return
		}
		opts.Tail = n
	}
	opts.Since = unixParam(q.Get("since"))
	opts.Until = unixParam(q.Get("until"))
	timestamps := boolParam(q.Get("timestamps"))
	tty := c.IsTTY() && !framed
	ct := "application/vnd.docker.multiplexed-stream"
	if tty {
		ct = "application/vnd.docker.raw-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	flush(w)
	st := newStreams(w, tty, func() { flush(w) })
	s.e.Logs(r.Context(), id, opts, func(le engine.LogEntry) error {
		line := le.Log
		if timestamps {
			line = le.Time.Format(time.RFC3339Nano) + " " + line
		}
		_, err := io.WriteString(st.of(le.Stream), line)
		return err
	})
}

func unixParam(v string) time.Time {
	if v == "" || v == "0" {
		return time.Time{}
	}
	sec, frac, _ := strings.Cut(v, ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}
	}
	var ns int64
	if frac != "" {
		frac = (frac + "000000000")[:9]
		ns, _ = strconv.ParseInt(frac, 10, 64)
	}
	return time.Unix(s, ns)
}

func (s *Server) containerAttach(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := r.PathValue("id")
	c, err := s.e.Container(id)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	tty := c.IsTTY()
	var conn net.Conn
	opts := attachOptions(q, boolParam(q.Get("stdin")), boolParam(q.Get("stdout")), boolParam(q.Get("stderr")))
	err = s.e.Attach(context.Background(), id, opts, func() (io.Reader, func(engine.LogEntry) error, error) {
		hc, rw, err := hijack(w, r, tty)
		if err != nil {
			return nil, nil, err
		}
		conn = hc
		st := newStreams(conn, tty, nil)
		return rw, func(le engine.LogEntry) error {
			_, err := io.WriteString(st.of(le.Stream), le.Log)
			return err
		}, nil
	})
	if conn == nil {
		if err != nil {
			writeEngineError(w, err)
		}
		return
	}
	closeWrite(conn)
	conn.Close()
}

func attachOptions(q url.Values, stdin, stdout, stderr bool) engine.AttachOptions {
	return engine.AttachOptions{
		Stream: boolParam(q.Get("stream")), Logs: boolParam(q.Get("logs")),
		Stdin: stdin, Stdout: stdout, Stderr: stderr, DetachKeys: q.Get("detachKeys"),
	}
}

// containerAttachWS attaches over a websocket, which carries each stream's
// output unframed, in binary messages from API 1.28 on and text ones before.
func (s *Server) containerAttachWS(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	version := apiVersion(r)
	// Before API 1.42 a websocket attach takes every stream.
	stdin, stdout, stderr := true, true, true
	if cmpVersion(version, "1.42") >= 0 {
		stdin, stdout, stderr = boolParam(q.Get("stdin")), boolParam(q.Get("stdout")), boolParam(q.Get("stderr"))
	}
	conns := make(chan *websocket.Conn)
	served := make(chan struct{})
	release := make(chan struct{})
	upgraded := false
	err := s.e.Attach(context.Background(), r.PathValue("id"), attachOptions(q, stdin, stdout, stderr), func() (io.Reader, func(engine.LogEntry) error, error) {
		upgraded = true
		srv := websocket.Server{Handler: func(ws *websocket.Conn) {
			conns <- ws
			<-release
			ws.Close()
		}}
		go func() {
			defer close(served)
			srv.ServeHTTP(w, r)
		}()
		select {
		case ws := <-conns:
			if cmpVersion(version, "1.28") >= 0 {
				ws.PayloadType = websocket.BinaryFrame
			}
			return ws, func(le engine.LogEntry) error {
				_, err := io.WriteString(ws, le.Log)
				return err
			}, nil
		case <-served:
			// The handshake failed, and the websocket server answered it.
			return nil, nil, errHandshake
		}
	})
	if !upgraded {
		writeEngineError(w, err)
		return
	}
	close(release)
	<-served
}

var errHandshake = errors.New("websocket handshake failed")

func (s *Server) execCreate(w http.ResponseWriter, r *http.Request) {
	var req container.ExecCreateRequest
	if !decode(w, r, &req) {
		return
	}
	id, err := s.e.ExecCreate(r.PathValue("id"), req)
	respondCode(w, http.StatusCreated, container.ExecCreateResponse{ID: id}, err)
}

func (s *Server) execStart(w http.ResponseWriter, r *http.Request) {
	var req container.ExecStartRequest
	if !decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if _, err := s.e.ExecInspect(id); err != nil {
		writeEngineError(w, err)
		return
	}
	if req.Detach {
		go s.e.ExecStart(id, req.Tty, req.ConsoleSize, engine.ExecIO{Stdout: io.Discard, Stderr: io.Discard})
		w.WriteHeader(http.StatusOK)
		return
	}
	conn, rw, err := hijack(w, r, req.Tty)
	if err != nil {
		return
	}
	defer conn.Close()
	st := newStreams(conn, req.Tty, nil)
	if err := s.e.ExecStart(id, req.Tty, req.ConsoleSize, engine.ExecIO{Stdin: rw, Stdout: st.stdout, Stderr: st.stderr}); err != nil {
		io.WriteString(st.stderr, err.Error()+"\n")
	}
	closeWrite(conn)
}

func (s *Server) networkCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string
		Driver     string
		Internal   bool
		Attachable bool
		EnableIPv4 *bool
		EnableIPv6 *bool
		IPAM       *network.IPAM
		Options    map[string]string
		Labels     map[string]string
	}
	if !decode(w, r, &req) {
		return
	}
	res, err := s.e.NetworkCreate(engine.NetworkCreate{
		Name: req.Name, Driver: req.Driver, Internal: req.Internal, Attachable: req.Attachable,
		EnableIPv4: req.EnableIPv4, EnableIPv6: req.EnableIPv6, IPAM: req.IPAM, Options: req.Options, Labels: req.Labels,
	})
	respondCode(w, http.StatusCreated, res, err)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := engine.ParseFilters(q.Get("filters"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	since, until := unixParam(q.Get("since")), unixParam(q.Get("until"))
	past, ch, cancel := s.e.Subscribe(since)
	defer cancel()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flush(w)
	enc := json.NewEncoder(w)
	send := func(m events.Message) bool {
		if !until.IsZero() && m.TimeNano > until.UnixNano() {
			return false
		}
		if matchEvent(f, m) {
			if enc.Encode(m) != nil {
				return false
			}
			flush(w)
		}
		return true
	}
	for _, m := range past {
		if !send(m) {
			return
		}
	}
	var deadline <-chan time.Time
	if !until.IsZero() {
		deadline = time.After(time.Until(until))
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			return
		case m := <-ch:
			if !send(m) {
				return
			}
		}
	}
}

func matchEvent(f engine.Filters, m events.Message) bool {
	in := func(key string, vals ...string) bool {
		if len(f[key]) == 0 {
			return true
		}
		for _, want := range f[key] {
			for _, v := range vals {
				if v != "" && (v == want || strings.HasPrefix(v, want) && key != "event") {
					return true
				}
			}
		}
		return false
	}
	a := m.Actor.Attributes
	if !in("type", string(m.Type)) || !in("event", string(m.Action), strings.SplitN(string(m.Action), ":", 2)[0]) {
		return false
	}
	if m.Type == events.ContainerEventType && !in("container", m.Actor.ID, a["name"]) {
		return false
	}
	if len(f["container"]) > 0 && m.Type != events.ContainerEventType {
		return false
	}
	if !in("image", a["image"]) || (m.Type == events.NetworkEventType && !in("network", m.Actor.ID, a["name"])) ||
		(m.Type == events.VolumeEventType && !in("volume", m.Actor.ID)) {
		return false
	}
	for _, want := range f["label"] {
		k, v, hasValue := strings.Cut(want, "=")
		got, ok := a[k]
		if !ok || hasValue && got != v {
			return false
		}
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes an error as Docker does, its message, and as Podman
// does, with the cause its clients match and the status: the body of
// Podman's compatible API too, whose extra fields Docker clients pass over.
func writeError(w http.ResponseWriter, status int, msg string) {
	cause := causeOf(msg)
	if _, ok := w.(libpodWriter); ok {
		msg = libpodMessage(msg)
	}
	writeJSON(w, status, errorBody{Message: msg, Cause: cause, Response: status})
}

type errorBody struct {
	Message  string `json:"message"`
	Cause    string `json:"cause"`
	Response int    `json:"response"`
}

// causes are the errors Podman's clients match by cause, by the message
// the engine gives them.
var causes = []struct{ prefix, cause string }{
	{"No such container", "no such container"},
	{"No such image", "no such image"},
	{"No such exec instance", "no such exec session"},
	{"No such pod", "no such pod"},
	{"network ", "network not found"},
}

func causeOf(msg string) string {
	for _, c := range causes {
		if strings.HasPrefix(msg, c.prefix) && (c.prefix != "network " || strings.HasSuffix(msg, " not found")) {
			return c.cause
		}
	}
	if strings.Contains(msg, "no such volume") {
		return "no such volume"
	}
	return msg
}

type unsupportedError string

func (u unsupportedError) Error() string { return string(u) }

func unsupportedErr(msg string) error { return unsupportedError(msg) }

func writeEngineError(w http.ResponseWriter, err error) {
	var u unsupportedError
	if errors.As(err, &u) {
		writeError(w, http.StatusNotImplemented, err.Error())
		return
	}
	switch engine.KindOf(err) {
	case engine.KindInvalid:
		writeError(w, http.StatusBadRequest, err.Error())
	case engine.KindNotFound:
		writeError(w, http.StatusNotFound, err.Error())
	case engine.KindConflict:
		writeError(w, http.StatusConflict, err.Error())
	case engine.KindNotModified:
		w.WriteHeader(http.StatusNotModified)
	case engine.KindUnsupported:
		writeError(w, http.StatusNotImplemented, err.Error())
	case engine.KindUnauthorized:
		writeError(w, http.StatusUnauthorized, err.Error())
	case engine.KindForbidden:
		writeError(w, http.StatusForbidden, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func respond(w http.ResponseWriter, v any, err error) { respondCode(w, http.StatusOK, v, err) }

func respondCode(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, status, v)
}

func respondStatus(w http.ResponseWriter, status int, err error) {
	if err != nil {
		writeEngineError(w, err)
		return
	}
	w.WriteHeader(status)
}

func (s *Server) containerStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stream := q.Get("stream") == "" || boolParam(q.Get("stream"))
	started := false
	enc := json.NewEncoder(w)
	err := s.e.Stats(r.Context(), r.PathValue("id"), stream, boolParam(q.Get("one-shot")), func(st container.StatsResponse) error {
		if !started {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if err := enc.Encode(st); err != nil {
			return err
		}
		flush(w)
		return nil
	})
	if err != nil && !started {
		writeEngineError(w, err)
	}
}

func (s *Server) commit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var cfg container.Config
	if !decode(w, r, &cfg) {
		return
	}
	opts := engine.CommitOptions{
		Container: q.Get("container"), Repo: q.Get("repo"), Tag: q.Get("tag"),
		Comment: q.Get("comment"), Author: q.Get("author"), Changes: q["changes"],
		Pause: q.Get("pause") == "" || boolParam(q.Get("pause")),
	}
	if r.ContentLength != 0 {
		opts.Config = &cfg
	}
	id, err := s.e.Commit(r.Context(), opts)
	respondCode(w, http.StatusCreated, map[string]string{"Id": id}, err)
}

// buildPrune serves POST /build/prune: the classic builder's cache, which
// dockerd keeps as intermediate images, and BuildKit's.
func (s *Server) buildPrune(w http.ResponseWriter, r *http.Request) {
	report := build.CachePruneReport{CachesDeleted: []string{}}
	freed, err := s.e.BuildCachePrune()
	if err != nil {
		writeEngineError(w, err)
		return
	}
	report.SpaceReclaimed = freed
	if s.bk != nil {
		ids, n, err := s.bk.Prune(r.Context(), boolParam(r.URL.Query().Get("all")))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		report.CachesDeleted = append(report.CachesDeleted, ids...)
		report.SpaceReclaimed += n
	}
	writeJSON(w, http.StatusOK, report)
}

// systemDF serves GET /system/df in API 1.51's shape, the build cache
// holding the classic builder's steps and BuildKit's records.
func (s *Server) systemDF(w http.ResponseWriter, r *http.Request) {
	want := map[string]bool{}
	for _, t := range r.URL.Query()["type"] {
		switch t {
		case "image", "container", "volume", "build-cache":
			want[t] = true
		default:
			writeError(w, http.StatusBadRequest, "unknown object type: "+t)
			return
		}
	}
	u, err := s.e.DiskUsage(r.Context(), want)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if s.bk != nil && (len(want) == 0 || want["build-cache"]) {
		records, err := s.bk.DiskUsage(r.Context())
		if err != nil {
			writeEngineError(w, err)
			return
		}
		u.BuildCache = append(u.BuildCache, records...)
	}
	writeJSON(w, http.StatusOK, u)
}

// distribution serves GET /distribution/{name}/json with the client's
// credentials.
func (s *Server) distribution(w http.ResponseWriter, r *http.Request) {
	ref, ok := strings.CutSuffix(r.PathValue("ref"), "/json")
	if !ok {
		writeError(w, http.StatusNotFound, "page not found")
		return
	}
	auth, err := registryAuth(r.Header.Get("X-Registry-Auth"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.e.Distribution(r.Context(), ref, auth)
	respond(w, res, err)
}
