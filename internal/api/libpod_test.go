package api

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/moby/moby/api/types/container"
)

// The body podman --remote create sends (testdata/podman-create.json, as
// Podman 5.8 sent it for a create with most of its flags) becomes the
// Docker create request that does the same.
func TestDockerCreateFromPodmanSpec(t *testing.T) {
	b, err := os.ReadFile("testdata/podman-create.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec specGen
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	req, err := dockerCreate(spec)
	if err != nil {
		t.Fatal(err)
	}
	cfg, hc := req.Config, req.HostConfig
	if cfg.Image != "lp/tiny:1" || !reflect.DeepEqual([]string(cfg.Cmd), []string{"/bin/true"}) || !reflect.DeepEqual(cfg.Env, []string{"A=1"}) ||
		cfg.Labels["k"] != "v" || cfg.User != "1000:1000" || cfg.WorkingDir != "/work" || cfg.Hostname != "h1" ||
		cfg.StopSignal != "SIGINT" || cfg.StopTimeout == nil || *cfg.StopTimeout != 7 {
		t.Errorf("config: %+v", cfg)
	}
	if h := cfg.Healthcheck; h == nil || !reflect.DeepEqual(h.Test, []string{"CMD-SHELL", "true"}) || h.Interval.Seconds() != 2 || h.Retries != 3 {
		t.Errorf("healthcheck: %+v", cfg.Healthcheck)
	}
	var ports []string
	for p, bs := range hc.PortBindings {
		for _, b := range bs {
			ports = append(ports, p.String()+"="+b.HostPort)
		}
	}
	sort.Strings(ports)
	if want := []string{"80/tcp=8080", "9000/udp=9000", "9001/udp=9001"}; !reflect.DeepEqual(ports, want) {
		t.Errorf("ports %v, want %v", ports, want)
	}
	if hc.Memory != 64<<20 || hc.MemorySwap != 0 || hc.CPUQuota != 150000 || hc.CPUPeriod != 100000 || hc.PidsLimit == nil || *hc.PidsLimit != 50 {
		t.Errorf("resources: memory %d swap %d quota %d period %d pids %v", hc.Memory, hc.MemorySwap, hc.CPUQuota, hc.CPUPeriod, hc.PidsLimit)
	}
	if len(hc.Ulimits) != 1 || hc.Ulimits[0].Name != "nofile" || hc.Ulimits[0].Soft != 1024 || hc.Ulimits[0].Hard != 2048 {
		t.Errorf("ulimits: %+v", hc.Ulimits)
	}
	if hc.RestartPolicy.Name != container.RestartPolicyOnFailure || hc.RestartPolicy.MaximumRetryCount != 3 || hc.Init == nil || !*hc.Init ||
		!hc.ReadonlyRootfs || hc.ShmSize != 8<<20 || hc.IpcMode != "shareable" || !reflect.DeepEqual(hc.GroupAdd, []string{"777"}) ||
		!reflect.DeepEqual(hc.ExtraHosts, []string{"db:10.0.0.5"}) || !reflect.DeepEqual(hc.CapDrop, []string{"ALL"}) {
		t.Errorf("host config: %+v", hc)
	}
	if hc.NetworkMode != "lpn2" || req.NetworkingConfig == nil || !reflect.DeepEqual(req.NetworkingConfig.EndpointsConfig["lpn2"].Aliases, []string{"web"}) {
		t.Errorf("network: %s %+v", hc.NetworkMode, req.NetworkingConfig)
	}
	if _, ok := hc.Tmpfs["/scratch"]; !ok || len(hc.Mounts) != 2 || hc.Mounts[0].Source != "/host/dir" || !hc.Mounts[0].ReadOnly ||
		hc.Mounts[1].Type != "volume" || hc.Mounts[1].Source != "v1" || hc.Mounts[1].Target != "/data" {
		t.Errorf("mounts: %+v tmpfs %v", hc.Mounts, hc.Tmpfs)
	}
}

// Podman's paths, versioned by its releases, reach the libpod handlers
// and the Docker handlers they share, and answer with Podman's headers.
func TestLibpodRoutes(t *testing.T) {
	ts := newServer(t)
	res := call(t, ts, "GET", "/v5.8.9/libpod/_ping", "", nil)
	if res.StatusCode != 200 || res.Header.Get("Libpod-API-Version") != LibpodAPIVersion {
		t.Fatalf("ping: %d %v", res.StatusCode, res.Header)
	}
	var e errorBody
	if res := call(t, ts, "GET", "/v5.8.9/libpod/containers/nope/json", "", &e); res.StatusCode != 404 || e.Cause != "no such container" ||
		e.Message != `no container with name or ID "nope" found: no such container` {
		t.Errorf("a missing container: %d %+v", res.StatusCode, e)
	}
	if res := call(t, ts, "GET", "/v5.8.9/libpod/containers/nope/logs", "", &e); res.StatusCode != 404 || e.Message != `no container with name or ID "nope" found: no such container` {
		t.Errorf("logs, which Docker's handler serves: %d %+v", res.StatusCode, e)
	}
	if res := call(t, ts, "GET", "/v1.44/containers/nope/json", "", &e); res.StatusCode != 404 || e.Message != "No such container: nope" {
		t.Errorf("a missing container on Docker's path: %d %+v", res.StatusCode, e)
	}
	if res := call(t, ts, "POST", "/v6.1.4/libpod/exec/nope/remove", `{"Force":false}`, &e); res.StatusCode != 404 || e.Cause != "no such exec session" {
		t.Errorf("removing a missing exec session: %d %+v", res.StatusCode, e)
	}
	var pod map[string]string
	if res := call(t, ts, "POST", "/v5.8.9/libpod/pods/create", `{"name":"p","no_infra":true}`, &pod); res.StatusCode != http.StatusCreated || pod["Id"] == "" {
		t.Fatalf("pod create: %d %v", res.StatusCode, pod)
	}
	if res := call(t, ts, "POST", "/v5.8.9/libpod/pods/create", `{"name":"q"}`, &e); res.StatusCode != http.StatusNotImplemented {
		t.Errorf("a pod with an infra container: %d %+v", res.StatusCode, e)
	}
	if res := call(t, ts, "GET", "/v5.8.9/libpod/pods/p/exists", "", nil); res.StatusCode != http.StatusNoContent {
		t.Errorf("pod exists: %d", res.StatusCode)
	}
	if res := call(t, ts, "GET", "/v5.8.9/libpod/pods//exists", "", nil); res.StatusCode == http.StatusNoContent {
		t.Error("an empty pod name exists")
	}
	if res := call(t, ts, "DELETE", "/v5.8.9/libpod/pods/p", "", nil); res.StatusCode != 200 {
		t.Errorf("pod rm: %d", res.StatusCode)
	}
}
