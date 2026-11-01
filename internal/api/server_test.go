package api

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

func newServer(t *testing.T) *httptest.Server {
	ts, _ := newEngineServer(t)
	return ts
}

func newEngineServer(t *testing.T) (*httptest.Server, *engine.Engine) {
	t.Helper()
	e, err := engine.Open(engine.Config{Root: t.TempDir(), PortBase: 42000, PortLimit: 42100, BlockSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(e, nil))
	t.Cleanup(ts.Close)
	return ts, e
}

// loadImage stores a one-layer image for the engine's platform under tag.
func loadImage(t *testing.T, e *engine.Engine, tag string) {
	t.Helper()
	layer, err := random.Layer(64, types.DockerLayer)
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := img.ConfigFile()
	cfg.OS, cfg.Architecture = image.Platform.OS, image.Platform.Architecture
	cfg.Config.Cmd = []string{"/bin/true"}
	if img, err = mutate.ConfigFile(img, cfg); err != nil {
		t.Fatal(err)
	}
	ref, _ := name.NewTag(tag)
	var buf bytes.Buffer
	if err := tarball.Write(ref, img, &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Images().Load(&buf); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, ts *httptest.Server, method, path, body string, out any) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return res
}

func TestPingAndVersionNegotiation(t *testing.T) {
	ts := newServer(t)
	res := call(t, ts, "GET", "/_ping", "", nil)
	if res.StatusCode != 200 || res.Header.Get("Api-Version") != engine.APIVersion {
		t.Fatalf("ping: %d, Api-Version %q", res.StatusCode, res.Header.Get("Api-Version"))
	}
	if res := call(t, ts, "HEAD", "/_ping", "", nil); res.StatusCode != 200 {
		t.Fatalf("HEAD ping: %d", res.StatusCode)
	}
	var v map[string]any
	if res := call(t, ts, "GET", "/v1.44/version", "", &v); res.StatusCode != 200 || v["ApiVersion"] != engine.APIVersion {
		t.Fatalf("versioned /version: %d %v", res.StatusCode, v)
	}
	var e map[string]string
	if res := call(t, ts, "GET", "/v9.99/version", "", &e); res.StatusCode != 400 || !strings.Contains(e["message"], "too new") {
		t.Fatalf("too-new version: %d %v", res.StatusCode, e)
	}
}

func TestNetworkLifecycle(t *testing.T) {
	ts := newServer(t)
	var created network.CreateResponse
	body := `{"Name":"proj_default","Driver":"bridge","Labels":{"com.docker.compose.project":"proj"}}`
	if res := call(t, ts, "POST", "/networks/create", body, &created); res.StatusCode != 201 || created.ID == "" {
		t.Fatalf("create: %d %+v", res.StatusCode, created)
	}
	if res := call(t, ts, "POST", "/networks/create", body, nil); res.StatusCode != 409 {
		t.Fatalf("duplicate create: %d, want 409", res.StatusCode)
	}
	var list []network.Summary
	call(t, ts, "GET", `/networks?filters={"label":{"com.docker.compose.project=proj":true}}`, "", &list)
	if len(list) != 1 || list[0].Name != "proj_default" {
		t.Fatalf("filtered list: %+v", list)
	}
	var insp network.Inspect
	if res := call(t, ts, "GET", "/networks/proj_default", "", &insp); res.StatusCode != 200 || insp.ID != created.ID {
		t.Fatalf("inspect by name: %d %+v", res.StatusCode, insp)
	}
	if res := call(t, ts, "DELETE", "/networks/bridge", "", nil); res.StatusCode != 409 {
		t.Fatalf("removing predefined network: %d", res.StatusCode)
	}
	if res := call(t, ts, "DELETE", "/networks/"+created.ID, "", nil); res.StatusCode != 204 {
		t.Fatalf("remove: %d", res.StatusCode)
	}
	if res := call(t, ts, "GET", "/networks/proj_default", "", nil); res.StatusCode != 404 {
		t.Fatalf("inspect removed: %d", res.StatusCode)
	}
	if res := call(t, ts, "POST", "/networks/create", `{"Name":"x","Driver":"overlay"}`, nil); res.StatusCode != 501 {
		t.Fatalf("overlay driver: %d, want 501", res.StatusCode)
	}
}

func TestVolumeLifecycle(t *testing.T) {
	ts := newServer(t)
	var v volume.Volume
	if res := call(t, ts, "POST", "/volumes/create", `{"Name":"proj_data","Labels":{"a":"b"}}`, &v); res.StatusCode != 201 || v.Name != "proj_data" || v.Driver != "local" {
		t.Fatalf("create: %d %+v", res.StatusCode, v)
	}
	var list volume.ListResponse
	call(t, ts, "GET", `/volumes?filters={"label":["a=b"]}`, "", &list)
	if len(list.Volumes) != 1 {
		t.Fatalf("list: %+v", list)
	}
	if res := call(t, ts, "DELETE", "/volumes/proj_data", "", nil); res.StatusCode != 204 {
		t.Fatalf("remove: %d", res.StatusCode)
	}
	if res := call(t, ts, "GET", "/volumes/proj_data", "", nil); res.StatusCode != 404 {
		t.Fatalf("inspect removed: %d", res.StatusCode)
	}
}

func TestContainerErrors(t *testing.T) {
	ts := newServer(t)
	var e map[string]string
	if res := call(t, ts, "GET", "/containers/nope/json", "", &e); res.StatusCode != 404 || !strings.Contains(e["message"], "No such container") {
		t.Fatalf("missing container: %d %v", res.StatusCode, e)
	}
	if res := call(t, ts, "POST", "/containers/create?name=x", `{"Image":"missing:1"}`, &e); res.StatusCode != 404 || !strings.Contains(e["message"], "No such image") {
		t.Fatalf("missing image: %d %v", res.StatusCode, e)
	}
	if res := call(t, ts, "POST", "/containers/create", `{"Image":"x","HostConfig":{"Memory":1048576}}`, &e); res.StatusCode != 501 || !strings.Contains(e["message"], "memory limits") {
		t.Fatalf("memory limit: %d %v", res.StatusCode, e)
	}
	if res := call(t, ts, "POST", "/containers/create", `{"Image":"x","HostConfig":{"NetworkMode":"host"}}`, &e); res.StatusCode != 501 {
		t.Fatalf("host network: %d %v", res.StatusCode, e)
	}
	var list []any
	if res := call(t, ts, "GET", "/containers/json?all=1", "", &list); res.StatusCode != 200 || len(list) != 0 {
		t.Fatalf("list: %d %v", res.StatusCode, list)
	}
}

// The docker CLI sends its endpoint configuration keyed by network mode
// "default", which Docker treats as "bridge". A 404 there reads to the CLI as
// a missing image.
func TestCreateOnDefaultNetwork(t *testing.T) {
	ts, e := newEngineServer(t)
	loadImage(t, e, "fws/tiny:1")
	var res map[string]any
	r := call(t, ts, "POST", "/containers/create?name=tiny", `{"Image":"fws/tiny:1","HostConfig":{"NetworkMode":"default"},"NetworkingConfig":{"EndpointsConfig":{"default":{}}}}`, &res)
	if r.StatusCode == 404 {
		t.Fatalf("create on the default network: 404 %v", res)
	}
	if msg, _ := res["message"].(string); strings.Contains(msg, "network") {
		t.Fatalf("create on the default network: %d %v", r.StatusCode, res)
	}
}

// The build context arrives while progress already streams back, so the
// handler must keep reading the upload after it starts responding.
func TestBuildReadsContextWhileStreaming(t *testing.T) {
	ts, e := newEngineServer(t)
	loadImage(t, e, "fws/tiny:1")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	df := []byte("FROM fws/tiny:1\nCOPY big.bin /big.bin\n")
	tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(df))})
	tw.Write(df)
	big := bytes.Repeat([]byte("x"), 4<<10)
	tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o644, Size: int64(len(big))})
	tw.Write(big)
	tw.Close()
	// The Docker CLI streams the context with chunked encoding, without a
	// length; hiding the buffer's length makes the client do the same.
	res, err := http.Post(ts.URL+"/build?t=x:1", "application/x-tar", io.MultiReader(&buf))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	if strings.Contains(string(out), "closed Body") || strings.Contains(string(out), "read build context") {
		t.Fatalf("build lost its context upload:\n%s", out)
	}
	if !strings.Contains(string(out), "Step 1/2 : FROM fws/tiny:1") {
		t.Fatalf("build did not start:\n%s", out)
	}
}
