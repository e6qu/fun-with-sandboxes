package api

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// The engine serves Podman's own API, libpod, beside Docker's, as Podman
// serves Docker's beside its own: under /libpod, versioned by Podman's
// releases, for podman --remote and the tools built on its bindings. Its
// endpoints that speak Docker's protocol pass to Docker's handlers; the
// rest take and give Podman's shapes (libpod_*.go).

// LibpodAPIVersion is the Podman release whose API the engine serves.
const LibpodAPIVersion = "5.8.0"

var libpodPrefix = regexp.MustCompile(`^/v\d+(?:\.\d+){1,2}/libpod(/.*)$`)

// libpodPath is the libpod path a request names, without its version.
func libpodPath(p string) (string, bool) {
	if m := libpodPrefix.FindStringSubmatch(p); m != nil {
		return m[1], true
	}
	if rest, ok := strings.CutPrefix(p, "/libpod/"); ok {
		return "/" + rest, true
	}
	return "", false
}

func (s *Server) serveLibpod(w http.ResponseWriter, r *http.Request, p string) {
	w.Header().Set("Libpod-API-Version", LibpodAPIVersion)
	w.Header().Set("Api-Version", engine.APIVersion)
	w.Header().Set("Server", "Libpod/"+LibpodAPIVersion+" (fun-with-sandboxes "+engine.Version+")")
	r.URL.Path, r.URL.RawPath = p, ""
	w = libpodWriter{w}
	if _, pattern := s.lp.Handler(r); pattern != "" {
		s.lp.ServeHTTP(w, r)
		return
	}
	// The endpoints libpod shares with Docker's API.
	s.mux.ServeHTTP(w, r)
}

// libpodWriter answers a libpod path's errors in libpod's words, whose
// text Podman's clients match: podman run --rm takes a container already
// gone for removed by "no such container" in the message.
type libpodWriter struct{ http.ResponseWriter }

func (w libpodWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w libpodWriter) Flush() { http.NewResponseController(w.ResponseWriter).Flush() }

func (w libpodWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// libpodMessage is libpod's message for the engine's Docker-worded one.
func libpodMessage(msg string) string {
	if ref, ok := strings.CutPrefix(msg, "No such container: "); ok {
		return fmt.Sprintf("no container with name or ID %q found: no such container", ref)
	}
	if id, ok := strings.CutPrefix(msg, "No such exec instance: "); ok {
		return fmt.Sprintf("no exec session with ID %s found: no such exec session", id)
	}
	return msg
}

func (s *Server) libpodRoutes() {
	m := s.lp
	m.HandleFunc("GET /_ping", s.ping)
	m.HandleFunc("HEAD /_ping", s.ping)
	m.HandleFunc("GET /version", s.libpodVersion)
	m.HandleFunc("GET /info", s.libpodInfo)
	s.libpodContainerRoutes()
	s.libpodImageRoutes()
	s.libpodNetworkRoutes()
	s.libpodVolumeRoutes()
	s.libpodPodRoutes()
}

func (s *Server) libpodVersion(w http.ResponseWriter, r *http.Request) {
	v := s.e.Version()
	v.APIVersion, v.MinAPIVersion = LibpodAPIVersion, "4.0.0"
	writeJSON(w, http.StatusOK, v)
}

// libpodInfo is podman info's answer: the engine's host and store, in
// Podman's shape, for a remote client.
func (s *Server) libpodInfo(w http.ResponseWriter, r *http.Request) {
	i := s.e.Info()
	host, _ := os.Hostname()
	socket := r.Context().Value(http.LocalAddrContextKey)
	path := ""
	if a, ok := socket.(interface{ String() string }); ok {
		path = a.String()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"host": map[string]any{
			"arch": runtime.GOARCH, "os": runtime.GOOS, "hostname": host, "kernel": i.KernelVersion,
			"cpus": i.NCPU, "memTotal": i.MemTotal, "memFree": 0, "swapFree": 0, "swapTotal": 0,
			"distribution":    map[string]string{"distribution": i.OperatingSystem, "version": i.OSVersion},
			"ociRuntime":      map[string]string{"name": "fwsd", "package": "fun-with-sandboxes", "path": "", "version": engine.Version},
			"remoteSocket":    map[string]any{"path": "unix://" + path, "exists": true},
			"security":        map[string]any{"rootless": true, "apparmorEnabled": false, "selinuxEnabled": false, "seccompEnabled": false, "capabilities": ""},
			"logDriver":       "json-file",
			"eventLogger":     "journald",
			"networkBackend":  "fwsd",
			"cgroupManager":   "none",
			"cgroupVersion":   "none",
			"uptime":          time.Since(s.started).Truncate(time.Second).String(),
			"serviceIsRemote": true,
			"idMappings":      map[string]any{"gidmap": nil, "uidmap": nil},
			"conmon":          map[string]string{"package": "", "path": "", "version": ""},
			"slirp4netns":     map[string]string{"executable": "", "package": "", "version": ""},
			"pasta":           map[string]string{"executable": "", "package": "", "version": ""},
			"databaseBackend": "fwsd",
		},
		"store": map[string]any{
			"containerStore":  map[string]int{"number": i.Containers, "paused": i.ContainersPaused, "running": i.ContainersRunning, "stopped": i.ContainersStopped},
			"imageStore":      map[string]int{"number": i.Images},
			"graphDriverName": "apfs-clone",
			"graphRoot":       i.DockerRootDir,
			"runRoot":         i.DockerRootDir,
			"volumePath":      i.DockerRootDir + "/volumes",
			"graphOptions":    map[string]any{},
			"graphStatus":     map[string]string{},
		},
		"registries": map[string]any{},
		"plugins": map[string][]string{
			"volume": {"local"}, "network": {"bridge"}, "log": {"json-file", "local"}, "authorization": nil,
		},
		"version": map[string]any{
			"APIVersion": LibpodAPIVersion, "Version": engine.Version, "GoVersion": runtime.Version(),
			"OsArch": runtime.GOOS + "/" + runtime.GOARCH, "Os": runtime.GOOS,
		},
	})
}
