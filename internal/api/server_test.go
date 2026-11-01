package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	e, err := engine.Open(engine.Config{Root: t.TempDir(), PortBase: 42000, PortLimit: 42100, BlockSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(e))
	t.Cleanup(ts.Close)
	return ts
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
