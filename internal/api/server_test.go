package api

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
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
	cfg.Created = v1.Time{Time: time.Now().UTC()}
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
	if res := call(t, ts, "POST", "/containers/create", `{"Image":"x","HostConfig":{"Memory":1048576}}`, &e); res.StatusCode != 400 || !strings.Contains(e["message"], "Minimum memory limit allowed is 6MB") {
		t.Fatalf("memory limit below Docker's minimum: %d %v", res.StatusCode, e)
	}
	if res := call(t, ts, "POST", "/containers/create", `{"Image":"x","HostConfig":{"CpusetCpus":"0"}}`, &e); res.StatusCode != 501 || !strings.Contains(e["message"], "cpusets") {
		t.Fatalf("cpuset: %d %v", res.StatusCode, e)
	}
	if res := call(t, ts, "POST", "/containers/create", `{"Image":"x","HostConfig":{"Ulimits":[{"Name":"nproc","Soft":10,"Hard":10}]}}`, &e); res.StatusCode != 501 || !strings.Contains(e["message"], "nproc") {
		t.Fatalf("ulimit nproc: %d %v", res.StatusCode, e)
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

func TestSaveHistoryAndPush(t *testing.T) {
	ts, e := newEngineServer(t)
	loadImage(t, e, "fws/tiny:1")

	res, err := http.Get(ts.URL + "/images/get?names=fws/tiny:1")
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/x-tar" {
		t.Fatalf("save: %d %s", res.StatusCode, archive)
	}
	other, err := engine.Open(engine.Config{Root: t.TempDir(), PortBase: 42100, PortLimit: 42200, BlockSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := other.Images().Load(bytes.NewReader(archive))
	if err != nil || len(loaded) != 1 || loaded[0].RepoTags[0] != "fws/tiny:1" {
		t.Fatalf("loading the saved archive: %+v, %v", loaded, err)
	}
	if res := call(t, ts, "GET", "/images/get?names=missing:1", "", nil); res.StatusCode != 404 {
		t.Fatalf("saving a missing image: %d", res.StatusCode)
	}

	var history []map[string]any
	if res := call(t, ts, "GET", "/images/fws/tiny:1/history", "", &history); res.StatusCode != 200 || len(history) == 0 {
		t.Fatalf("history: %d %v", res.StatusCode, history)
	}

	reg := httptest.NewServer(registry.New())
	defer reg.Close()
	host := strings.TrimPrefix(reg.URL, "http://")
	if res := call(t, ts, "POST", "/images/fws/tiny:1/tag?repo="+host+"/e2e/tiny&tag=pushed", "", nil); res.StatusCode != 201 {
		t.Fatalf("tag: %d", res.StatusCode)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/images/"+host+"/e2e/tiny/push?tag=pushed", nil)
	req.Header.Set("X-Registry-Auth", "e30=")
	pres, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(pres.Body)
	pres.Body.Close()
	if strings.Contains(string(out), `"error"`) || !strings.Contains(string(out), "digest: sha256:") {
		t.Fatalf("push stream:\n%s", out)
	}
	pulled, err := remote.Image(mustRef(t, host+"/e2e/tiny:pushed"))
	if err != nil {
		t.Fatalf("pushed image is not in the registry: %v", err)
	}
	if cfg, _ := pulled.ConfigFile(); cfg.OS != image.Platform.OS {
		t.Fatalf("pushed image config %+v", cfg)
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

// The pull stream reports each layer, the resolved digest and whether the
// image changed, as dockerd's does; a repeat pull is up to date.
func TestPullStreamAndDigestReferences(t *testing.T) {
	ts, e := newEngineServer(t)
	reg := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer reg.Close()
	host := strings.TrimPrefix(reg.URL, "http://")
	layer, _ := random.Layer(64, types.OCILayer)
	img, _ := mutate.AppendLayers(empty.Image, layer)
	cfg, _ := img.ConfigFile()
	cfg.OS, cfg.Architecture = image.Platform.OS, image.Platform.Architecture
	img, _ = mutate.ConfigFile(img, cfg)
	if err := remote.Write(mustRef(t, host+"/e2e/p:1"), img); err != nil {
		t.Fatal(err)
	}
	digest, _ := img.Digest()

	pull := func() string {
		res, err := http.Post(ts.URL+"/images/create?fromImage="+host+"/e2e/p&tag=1", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	out := pull()
	for _, want := range []string{`"status":"Pulling from e2e/p","id":"1"`, `"status":"Pulling fs layer"`, `"status":"Pull complete"`, `"status":"Digest: ` + digest.String() + `"`, `Status: Downloaded newer image for ` + host + `/e2e/p:1`} {
		if !strings.Contains(out, want) {
			t.Fatalf("pull stream lacks %s:\n%s", want, out)
		}
	}
	if out := pull(); !strings.Contains(out, "Status: Image is up to date for "+host+"/e2e/p:1") || strings.Contains(out, "Pulling fs layer") {
		t.Fatalf("repeat pull stream:\n%s", out)
	}

	ref := host + "/e2e/p@" + digest.String()
	var inspect struct{ RepoDigests []string }
	if res := call(t, ts, "GET", "/images/"+ref+"/json", "", &inspect); res.StatusCode != 200 || len(inspect.RepoDigests) != 1 || inspect.RepoDigests[0] != ref {
		t.Fatalf("inspect by digest: %d %+v", res.StatusCode, inspect)
	}
	var list []struct{ RepoDigests []string }
	call(t, ts, "GET", `/images/json?filters={"reference":["`+host+`/e2e/p"]}`, "", &list)
	if len(list) != 1 || len(list[0].RepoDigests) != 1 {
		t.Fatalf("image list %+v", list)
	}
	var removed []map[string]string
	if res := call(t, ts, "DELETE", "/images/"+host+"/e2e/p:1", "", &removed); res.StatusCode != 200 || len(removed) != 3 || removed[1]["Untagged"] != ref {
		t.Fatalf("removing the last tag: %d %v", res.StatusCode, removed)
	}
	if _, err := e.Images().Get(ref); err == nil {
		t.Fatal("the digest reference survived its image")
	}
}

// Prune removes dangling images by default and every unused image with
// dangling=false, leaving images containers use.
func TestImagePrune(t *testing.T) {
	ts, e := newEngineServer(t)
	loadImage(t, e, "prune/a:1")
	loadImage(t, e, "prune/a:1") // the first image is now dangling
	loadImage(t, e, "prune/b:1")
	var report struct {
		ImagesDeleted  []map[string]string
		SpaceReclaimed uint64
	}
	if res := call(t, ts, "POST", "/images/prune", "", &report); res.StatusCode != 200 || len(report.ImagesDeleted) != 1 || report.SpaceReclaimed == 0 {
		t.Fatalf("pruning dangling images: %d %+v", res.StatusCode, report)
	}
	if n := len(e.Images().List()); n != 2 {
		t.Fatalf("%d images left after pruning dangling ones, want 2", n)
	}
	var kept, all struct{ ImagesDeleted []map[string]string }
	call(t, ts, "POST", `/images/prune?filters={"dangling":["false"],"until":["2000-01-01"]}`, "", &kept)
	if len(kept.ImagesDeleted) != 0 {
		t.Fatalf("until removed newer images: %+v", kept)
	}
	call(t, ts, "POST", `/images/prune?filters={"dangling":["false"],"until":["1h"]}`, "", &kept)
	if len(kept.ImagesDeleted) != 0 {
		t.Fatalf("until an hour ago removed images made now: %+v", kept)
	}
	call(t, ts, "POST", `/images/prune?filters={"dangling":["false"]}`, "", &all)
	if n := len(e.Images().List()); n != 0 || len(all.ImagesDeleted) != 4 {
		t.Fatalf("pruning every unused image left %d, reported %+v", n, all)
	}
}

// A reference filter lists only the references it matched, as dockerd does.
func TestImageListReferenceFilterTrimsReferences(t *testing.T) {
	ts, e := newEngineServer(t)
	loadImage(t, e, "trim/a:1")
	if err := e.Images().Tag("trim/a:1", "other/b:2"); err != nil {
		t.Fatal(err)
	}
	var list []struct{ RepoTags []string }
	call(t, ts, "GET", `/images/json?filters={"reference":["other/*"]}`, "", &list)
	if len(list) != 1 || len(list[0].RepoTags) != 1 || list[0].RepoTags[0] != "other/b:2" {
		t.Fatalf("filtered list %+v", list)
	}
}
