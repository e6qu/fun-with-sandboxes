package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/e6qu/fun-with-sandboxes/internal/cri"
	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// A pod is Podman's group of containers. The engine keeps the pods of
// Podman's API by name and ID, and each member container carries its
// pod's ID in podLabel, Podman's own label for a pod's containers being
// its record. A pod's infra container holds what its members share: it
// runs the pause image, as a CRI pod sandbox does, and a member joins its
// network, and with it its hostname and published ports, its IPC and its
// PID namespace, as the pod shares each.
const podLabel = "io.podman.pod"

type pod struct {
	ID      string            `json:"Id"`
	Name    string            `json:"Name"`
	Created time.Time         `json:"Created"`
	Labels  map[string]string `json:"Labels"`
	// InfraID is the pod's infra container, and Share the namespaces its
	// members take from it: of ipc, net, uts and pid.
	InfraID string   `json:"InfraId,omitempty"`
	Share   []string `json:"Share,omitempty"`
}

func (p pod) shares(ns string) bool {
	for _, s := range p.Share {
		if s == ns {
			return true
		}
	}
	return false
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

// setInfra records the pod's infra container and what its members share.
func (ps *podStore) setInfra(id, infra string, share []string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p, ok := ps.pods[id]
	if !ok {
		return &engine.Error{Kind: engine.KindNotFound, Msg: "No such pod: " + id}
	}
	p.InfraID, p.Share = infra, share
	return ps.saveLocked()
}

// infraOf is the infra container of the pod with id, if it has one.
func (ps *podStore) infraOf(id string) string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p, ok := ps.pods[id]; ok {
		return p.InfraID
	}
	return ""
}

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
	m.HandleFunc("POST /pods/{name}/start", s.podEach(true, func(id string) error { return s.e.Start(id) }))
	m.HandleFunc("POST /pods/{name}/stop", s.podEach(false, func(id string) error { return s.e.Stop(id, nil, "") }))
	// The members stop before the infra container whose network they are
	// on restarts, and start again on it.
	m.HandleFunc("POST /pods/{name}/restart", func(w http.ResponseWriter, r *http.Request) {
		p, err := s.pods.get(r.PathValue("name"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		if p.InfraID == "" {
			s.podEach(false, func(id string) error { return s.e.Restart(id, nil, "") })(w, r)
			return
		}
		var errs []error
		for _, fn := range []struct {
			infraFirst bool
			do         func(id string) error
		}{{false, func(id string) error { return s.e.Stop(id, nil, "") }}, {true, func(id string) error { return s.e.Start(id) }}} {
			errs = append(errs, s.podDo(p, fn.infraFirst, fn.do)...)
		}
		podReport(w, p, errs)
	})
	m.HandleFunc("POST /pods/{name}/kill", func(w http.ResponseWriter, r *http.Request) {
		sig := r.URL.Query().Get("signal")
		if sig == "" {
			sig = "SIGKILL"
		}
		s.podEach(false, func(id string) error { return s.e.Kill(id, sig) })(w, r)
	})
	m.HandleFunc("DELETE /pods/{name}", s.podRemove)
}

type podSpec struct {
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	NoInfra    bool              `json:"no_infra"`
	Share      []string          `json:"shared_namespaces"`
	InfraImage string            `json:"infra_image"`
	InfraName  string            `json:"infra_name"`
	Hostname   string            `json:"hostname"`
}

func (s *Server) podCreate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var spec podSpec
	// A pod's network settings are its infra container's, under the names
	// a container's own have in Podman's specification.
	var infra specGen
	if len(body) > 0 {
		if err := json.Unmarshal(body, &spec); err == nil {
			err = json.Unmarshal(body, &infra)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}
	if spec.NoInfra {
		if len(spec.Share) > 0 {
			writeError(w, http.StatusNotImplemented, "a pod without an infra container shares no namespaces: create it with --share=\"\"")
			return
		}
		p, err := s.pods.create(spec.Name, spec.Labels)
		respondCode(w, http.StatusCreated, map[string]string{"Id": p.ID}, err)
		return
	}
	for _, ns := range spec.Share {
		switch ns {
		case "ipc", "net", "uts", "pid":
		default:
			writeError(w, http.StatusNotImplemented, "a pod cannot share the "+ns+" namespace: containers here have none of their own")
			return
		}
	}
	p, err := s.pods.create(spec.Name, spec.Labels)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	infra.Image = spec.InfraImage
	if infra.Image == "" {
		infra.Image = cri.DefaultSandboxImage
	}
	infra.Name = spec.InfraName
	if infra.Name == "" {
		infra.Name = p.ID[:12] + "-infra"
	}
	infra.Hostname = spec.Hostname
	if infra.Hostname == "" {
		infra.Hostname = p.Name
	}
	infra.Labels = map[string]string{podLabel: p.ID}
	// What the members join.
	infra.IpcNS = namespace{NSMode: "shareable"}
	infra.Pod = ""
	req, err := dockerCreate(infra)
	var res container.CreateResponse
	if err == nil {
		res, err = s.e.Create(engine.CreateOptions{Name: infra.Name}, req)
	}
	if err == nil {
		err = s.pods.setInfra(p.ID, res.ID, spec.Share)
	}
	if err != nil {
		if res.ID != "" {
			s.e.Remove(res.ID, engine.RemoveOptions{Force: true})
		}
		s.pods.remove(p.ID)
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"Id": p.ID})
}

// joinPod gives a container created in a pod what the pod shares, of its
// infra container: each namespace the container's specification leaves
// to the pod.
func joinPod(p pod, spec *specGen) {
	if p.InfraID == "" {
		return
	}
	of := namespace{NSMode: "container", Value: p.InfraID}
	unset := func(ns namespace) bool { return ns.NSMode == "" || ns.NSMode == "default" }
	if p.shares("net") && unset(spec.NetNS) && len(spec.Networks) == 0 {
		spec.NetNS = of
	}
	if p.shares("ipc") && unset(spec.IpcNS) {
		spec.IpcNS = of
	}
	if p.shares("pid") && unset(spec.PidNS) {
		spec.PidNS = of
	}
}

// startInfra starts the infra container of the pod the container id is
// in, whose network the container joins as it starts, as Podman starts a
// pod's infra container with the pod's first container.
func (s *Server) startInfra(id string) error {
	in, err := s.e.Inspect(id)
	if err != nil || in.Config == nil {
		return nil
	}
	infra := s.pods.infraOf(in.Config.Labels[podLabel])
	if infra == "" || infra == in.ID {
		return nil
	}
	if err := s.e.Start(infra); err != nil && engine.KindOf(err) != engine.KindNotModified {
		return err
	}
	return nil
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
			"Cgroup": "", "Containers": cs, "Created": p.Created, "Id": p.ID, "InfraId": p.InfraID, "Name": p.Name,
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
		"CreateInfra": p.InfraID != "", "InfraContainerID": p.InfraID, "SharedNamespaces": p.Share,
		"NumContainers": len(members), "Containers": cs, "ExitPolicy": "continue",
		"Hostname": "", "CreateCgroup": false, "LockNumber": 0,
	})
}

// podDo does fn to each of the pod's containers: to its infra container
// first, as a start needs, whose network the others join, or last.
func (s *Server) podDo(p pod, infraFirst bool, fn func(id string) error) []error {
	var errs []error
	do := func(id string) {
		if err := fn(id); err != nil && engine.KindOf(err) != engine.KindNotModified {
			errs = append(errs, err)
		}
	}
	if infraFirst && p.InfraID != "" {
		do(p.InfraID)
	}
	for _, m := range s.podMembers(p) {
		if m.ID != p.InfraID {
			do(m.ID)
		}
	}
	if !infraFirst && p.InfraID != "" {
		do(p.InfraID)
	}
	return errs
}

func podReport(w http.ResponseWriter, p pod, errs []error) {
	if len(errs) > 0 {
		writeEngineError(w, errors.Join(errs...))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"Errs": []string{}, "Id": p.ID})
}

// podEach does fn to each of the pod's containers, and reports as
// Podman's pod start and stop do.
func (s *Server) podEach(infraFirst bool, fn func(id string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.pods.get(r.PathValue("name"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		podReport(w, p, s.podDo(p, infraFirst, fn))
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
	// The infra container goes with its pod, after the members on its
	// network.
	sort.SliceStable(members, func(i, j int) bool { return members[j].ID == p.InfraID && members[i].ID != p.InfraID })
	for _, m := range members {
		if !force && m.ID != p.InfraID {
			writeError(w, http.StatusConflict, "pod "+p.ID+" contains containers and cannot be removed: pod has containers")
			return
		}
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
