package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// A pod is Podman's group of containers. The engine keeps the pods of
// Podman's API by name and ID, and each member container carries its
// pod's ID in podLabel, Podman's own label for a pod's containers being
// its record. A pod shares no namespaces: an infra container, which
// shares them, runs Podman's pause image, which the engine does not have.
const podLabel = "io.podman.pod"

type pod struct {
	ID      string            `json:"Id"`
	Name    string            `json:"Name"`
	Created time.Time         `json:"Created"`
	Labels  map[string]string `json:"Labels"`
}

type podStore struct {
	mu   sync.Mutex
	path string
	pods map[string]*pod
}

func openPods(root string) *podStore {
	ps := &podStore{path: filepath.Join(root, "pods.json"), pods: map[string]*pod{}}
	if b, err := os.ReadFile(ps.path); err == nil {
		json.Unmarshal(b, &ps.pods)
	}
	return ps
}

func (ps *podStore) saveLocked() error {
	b, err := json.Marshal(ps.pods)
	if err != nil {
		return err
	}
	tmp := ps.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ps.path)
}

func (ps *podStore) getLocked(ref string) *pod {
	if ref == "" {
		return nil
	}
	if p, ok := ps.pods[ref]; ok {
		return p
	}
	var found *pod
	for _, p := range ps.pods {
		if p.Name == ref {
			return p
		}
		if strings.HasPrefix(p.ID, ref) {
			if found != nil {
				return nil
			}
			found = p
		}
	}
	return found
}

func (ps *podStore) get(ref string) (pod, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p := ps.getLocked(ref); p != nil {
		return *p, nil
	}
	return pod{}, &engine.Error{Kind: engine.KindNotFound, Msg: "No such pod: " + ref}
}

func (ps *podStore) name(id string) string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p, ok := ps.pods[id]; ok {
		return p.Name
	}
	return ""
}

// matches reports whether the pod with id is one refs name.
func (ps *podStore) matches(id string, refs []string) bool {
	if id == "" {
		return false
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, r := range refs {
		if p := ps.getLocked(r); p != nil && p.ID == id {
			return true
		}
	}
	return false
}

var podName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func (ps *podStore) create(name string, labels map[string]string) (pod, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	b := make([]byte, 32)
	rand.Read(b)
	id := hex.EncodeToString(b)
	if name == "" {
		name = "pod_" + id[:12]
	}
	if !podName.MatchString(name) {
		return pod{}, &engine.Error{Kind: engine.KindInvalid, Msg: "invalid pod name " + name}
	}
	for _, p := range ps.pods {
		if p.Name == name {
			return pod{}, &engine.Error{Kind: engine.KindConflict, Msg: name + ": pod already exists"}
		}
	}
	p := &pod{ID: id, Name: name, Created: time.Now().UTC(), Labels: labels}
	ps.pods[id] = p
	return *p, ps.saveLocked()
}

func (ps *podStore) remove(id string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.pods, id)
	return ps.saveLocked()
}

func (ps *podStore) list() []pod {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make([]pod, 0, len(ps.pods))
	for _, p := range ps.pods {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (s *Server) libpodPodRoutes() {
	m := s.lp
	m.HandleFunc("POST /pods/create", s.podCreate)
	m.HandleFunc("GET /pods/json", s.podList)
	m.HandleFunc("GET /pods/{name}/json", s.podInspect)
	m.HandleFunc("GET /pods/{name}/exists", func(w http.ResponseWriter, r *http.Request) {
		_, err := s.pods.get(r.PathValue("name"))
		respondStatus(w, http.StatusNoContent, err)
	})
	m.HandleFunc("POST /pods/{name}/start", s.podEach(func(id string) error { return s.e.Start(id) }))
	m.HandleFunc("POST /pods/{name}/stop", s.podEach(func(id string) error { return s.e.Stop(id, nil, "") }))
	m.HandleFunc("POST /pods/{name}/restart", s.podEach(func(id string) error { return s.e.Restart(id, nil, "") }))
	m.HandleFunc("POST /pods/{name}/kill", func(w http.ResponseWriter, r *http.Request) {
		sig := r.URL.Query().Get("signal")
		if sig == "" {
			sig = "SIGKILL"
		}
		s.podEach(func(id string) error { return s.e.Kill(id, sig) })(w, r)
	})
	m.HandleFunc("DELETE /pods/{name}", s.podRemove)
}

type podSpec struct {
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	NoInfra bool              `json:"no_infra"`
	Share   []string          `json:"shared_namespaces"`
}

func (s *Server) podCreate(w http.ResponseWriter, r *http.Request) {
	var spec podSpec
	if !decode(w, r, &spec) {
		return
	}
	if !spec.NoInfra {
		writeError(w, http.StatusNotImplemented, "pods with an infra container are not supported: the infra container runs Podman's pause image, which this engine does not have; create the pod with --infra=false")
		return
	}
	if len(spec.Share) > 0 {
		writeError(w, http.StatusNotImplemented, "a pod without an infra container shares no namespaces: create it with --share=\"\"")
		return
	}
	p, err := s.pods.create(spec.Name, spec.Labels)
	respondCode(w, http.StatusCreated, map[string]string{"Id": p.ID}, err)
}

// podMembers are the containers of the pod p, newest first.
func (s *Server) podMembers(p pod) []podMember {
	f := engine.Filters{"label": {podLabel + "=" + p.ID}}
	var out []podMember
	for _, c := range s.e.List(engine.ListOptions{All: true, Filters: f}) {
		out = append(out, podMember{ID: c.ID, Name: strings.TrimPrefix(firstName(c.Names), "/"), State: string(c.State), Status: c.Status})
	}
	return out
}

type podMember struct{ ID, Name, State, Status string }

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// podStatus is Podman's summary of its containers' states.
func podStatus(members []podMember) string {
	if len(members) == 0 {
		return "Created"
	}
	running, exited, paused := 0, 0, 0
	for _, m := range members {
		switch m.State {
		case "running":
			running++
		case "exited", "dead":
			exited++
		case "paused":
			paused++
		}
	}
	switch {
	case running == len(members):
		return "Running"
	case paused == len(members):
		return "Paused"
	case exited == len(members):
		return "Exited"
	case running > 0:
		return "Degraded"
	}
	return "Created"
}

func (s *Server) podList(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, p := range s.pods.list() {
		members := s.podMembers(p)
		cs := []map[string]any{}
		for _, m := range members {
			cs = append(cs, map[string]any{"Id": m.ID, "Names": m.Name, "Status": m.State, "RestartCount": 0})
		}
		out = append(out, map[string]any{
			"Cgroup": "", "Containers": cs, "Created": p.Created, "Id": p.ID, "InfraId": "", "Name": p.Name,
			"Namespace": "", "Networks": []string{}, "Status": podStatus(members), "Labels": p.Labels,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) podInspect(w http.ResponseWriter, r *http.Request) {
	p, err := s.pods.get(r.PathValue("name"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	members := s.podMembers(p)
	cs := []map[string]string{}
	for _, m := range members {
		cs = append(cs, map[string]string{"Id": m.ID, "Name": m.Name, "State": m.State})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Id": p.ID, "Name": p.Name, "Created": p.Created, "State": podStatus(members), "Labels": p.Labels,
		"CreateInfra": false, "NumContainers": len(members), "Containers": cs, "ExitPolicy": "continue",
		"Hostname": "", "CreateCgroup": false, "LockNumber": 0,
	})
}

// podEach does fn to each of the pod's containers, and reports as
// Podman's pod start and stop do.
func (s *Server) podEach(fn func(id string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.pods.get(r.PathValue("name"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		var errs []error
		for _, m := range s.podMembers(p) {
			if err := fn(m.ID); err != nil && engine.KindOf(err) != engine.KindNotModified {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 {
			writeEngineError(w, errors.Join(errs...))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"Errs": []string{}, "Id": p.ID})
	}
}

func (s *Server) podRemove(w http.ResponseWriter, r *http.Request) {
	p, err := s.pods.get(r.PathValue("name"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	force := boolParam(r.URL.Query().Get("force"))
	members := s.podMembers(p)
	if !force && len(members) > 0 {
		writeError(w, http.StatusConflict, "pod "+p.ID+" contains containers and cannot be removed: pod has containers")
		return
	}
	removed := map[string]any{}
	for _, m := range members {
		if err := s.e.Remove(m.ID, engine.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			writeEngineError(w, err)
			return
		}
		removed[m.ID] = nil
	}
	if err := s.pods.remove(p.ID); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"RemovedCtrs": removed, "Id": p.ID})
}
