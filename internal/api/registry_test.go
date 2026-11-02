package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const (
	regUser = "fws"
	regPass = "s3cret"
)

// authRegistry serves a registry that admits only regUser: with HTTP basic
// auth, or, with token set, through Docker's token flow, a Bearer challenge
// and a token endpoint that checks the credentials.
func authRegistry(t *testing.T, token bool) string {
	t.Helper()
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	const issued = "token-for-fws"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, basic := r.BasicAuth()
		if r.URL.Path == "/token" {
			if !basic || user != regUser || pass != regPass {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"token": issued})
			return
		}
		ok := basic && user == regUser && pass == regPass
		if token {
			ok = r.Header.Get("Authorization") == "Bearer "+issued
		}
		if !ok {
			if token {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test"`)
			} else {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func authHeader(t *testing.T, user, pass, server string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"username": user, "password": pass, "serveraddress": server})
	return base64.URLEncoding.EncodeToString(b)
}

func streamCall(t *testing.T, method, url, header string, body io.Reader) string {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	if header != "" {
		req.Header.Set("X-Registry-Auth", header)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

// docker login checks credentials against the registry, through basic auth
// or a token exchange, and fails with 401 when the registry refuses them.
func TestLoginChecksCredentials(t *testing.T) {
	ts := newServer(t)
	for _, token := range []bool{false, true} {
		host := authRegistry(t, token)
		var ok struct{ Status string }
		body := `{"username":"` + regUser + `","password":"` + regPass + `","serveraddress":"` + host + `"}`
		if res := call(t, ts, "POST", "/auth", body, &ok); res.StatusCode != 200 || ok.Status != "Login Succeeded" {
			t.Fatalf("token=%v: login with the right password: %d %+v", token, res.StatusCode, ok)
		}
		var refused struct{ Message string }
		body = `{"username":"` + regUser + `","password":"wrong","serveraddress":"` + host + `"}`
		if res := call(t, ts, "POST", "/auth", body, &refused); res.StatusCode != 401 || !strings.Contains(refused.Message, "401 Unauthorized") {
			t.Fatalf("token=%v: login with a wrong password: %d %+v", token, res.StatusCode, refused)
		}
	}
}

// Pulls and pushes use the credentials the client sends; a push reports
// each layer, and a layer the registry holds already is not sent again.
func TestPullAndPushWithClientCredentials(t *testing.T) {
	ts, e := newEngineServer(t)
	loadImage(t, e, "auth/src:1")
	host := authRegistry(t, true)
	good := authHeader(t, regUser, regPass, host)
	if err := e.Images().Tag("auth/src:1", host+"/r/img:1"); err != nil {
		t.Fatal(err)
	}
	if out := streamCall(t, "POST", ts.URL+"/images/"+host+"/r/img/push?tag=1", "", nil); !strings.Contains(out, `"error"`) {
		t.Fatalf("an anonymous push to a registry that needs credentials succeeded:\n%s", out)
	}
	out := streamCall(t, "POST", ts.URL+"/images/"+host+"/r/img/push?tag=1", good, nil)
	if strings.Contains(out, `"error"`) || !strings.Contains(out, `"status":"Preparing"`) || !strings.Contains(out, `"status":"Pushed"`) {
		t.Fatalf("push with credentials:\n%s", out)
	}
	if out := streamCall(t, "POST", ts.URL+"/images/"+host+"/r/img/push?tag=1", good, nil); !strings.Contains(out, "Layer already exists") || strings.Contains(out, `"status":"Pushed"`) {
		t.Fatalf("pushing again sent the layer again:\n%s", out)
	}
	if _, err := remote.Image(mustRef(t, host+"/r/img:1"), remote.WithAuth(&authn.Basic{Username: regUser, Password: regPass})); err != nil {
		t.Fatalf("the pushed image is not in the registry: %v", err)
	}

	pull := ts.URL + "/images/create?fromImage=" + url.QueryEscape(host+"/r/img") + "&tag=1"
	if out := streamCall(t, "POST", pull, "", nil); !strings.Contains(out, `"error"`) {
		t.Fatalf("an anonymous pull from a registry that needs credentials succeeded:\n%s", out)
	}
	if out := streamCall(t, "POST", pull, authHeader(t, regUser, "wrong", host), nil); !strings.Contains(out, `"error"`) {
		t.Fatalf("a pull with a wrong password succeeded:\n%s", out)
	}
	if out := streamCall(t, "POST", pull, good, nil); strings.Contains(out, `"error"`) || !strings.Contains(out, "Status: ") {
		t.Fatalf("pull with credentials:\n%s", out)
	}
}

func fsTarball(t *testing.T, gz bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = zw
	}
	tw := tar.NewWriter(w)
	tw.WriteHeader(&tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755})
	body := []byte("imported\n")
	tw.WriteHeader(&tar.Header{Name: "etc/hello", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	tw.Write(body)
	tw.Close()
	if zw != nil {
		zw.Close()
	}
	return buf.Bytes()
}

// docker import makes a one-layer image of a tarball from the request or a
// URL, with --change and --message applied.
func TestImport(t *testing.T) {
	ts, e := newEngineServer(t)
	q := "fromSrc=-&repo=imp/a&tag=1&message=from+a+test&changes=" + url.QueryEscape(`CMD ["/bin/hello"]`) + "&changes=" + url.QueryEscape("ENV A=1")
	out := streamCall(t, "POST", ts.URL+"/images/create?"+q, "", bytes.NewReader(fsTarball(t, true)))
	var msg struct{ Status string }
	if err := json.Unmarshal([]byte(out), &msg); err != nil || !strings.HasPrefix(msg.Status, "sha256:") {
		t.Fatalf("import stream %q: %v", out, err)
	}
	img, err := e.Images().Get("imp/a:1")
	if err != nil || img.ID != msg.Status {
		t.Fatalf("imported image %+v, %v; want %s", img, err, msg.Status)
	}
	if c := img.Config; len(c.Config.Cmd) != 1 || c.Config.Cmd[0] != "/bin/hello" || len(c.Config.Env) != 1 || c.Config.Env[0] != "A=1" ||
		len(c.History) != 1 || c.History[0].Comment != "from a test" || len(c.RootFS.DiffIDs) != 1 || c.OS != "darwin" {
		t.Fatalf("imported config %+v", c)
	}
	if b, err := os.ReadFile(filepath.Join(e.Images().Root(img.ID), "etc", "hello")); err != nil || string(b) != "imported\n" {
		t.Fatalf("the imported root holds %q, %v", b, err)
	}

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(fsTarball(t, false)) }))
	defer src.Close()
	out = streamCall(t, "POST", ts.URL+"/images/create?fromSrc="+url.QueryEscape(src.URL+"/fs.tar")+"&repo=imp/b", "", nil)
	if err := json.Unmarshal([]byte(out), &msg); err != nil || !strings.HasPrefix(msg.Status, "sha256:") {
		t.Fatalf("import from a URL %q: %v", out, err)
	}
	img, err = e.Images().Get("imp/b:latest")
	if err != nil || img.Config.History[0].Comment != "Imported from "+src.URL+"/fs.tar" {
		t.Fatalf("image imported from a URL: %+v, %v", img, err)
	}
}

// GET /distribution resolves a name in its registry with the client's
// credentials: the descriptor and the platforms behind it.
func TestDistributionInspect(t *testing.T) {
	ts, e := newEngineServer(t)
	loadImage(t, e, "dist/src:1")
	host := authRegistry(t, false)
	if err := e.Images().Tag("dist/src:1", host+"/r/dist:1"); err != nil {
		t.Fatal(err)
	}
	good := authHeader(t, regUser, regPass, host)
	streamCall(t, "POST", ts.URL+"/images/"+host+"/r/dist/push?tag=1", good, nil)
	img, _, _ := e.Images().V1Image("dist/src:1")
	want, _ := img.Digest()

	req, _ := http.NewRequest("GET", ts.URL+"/distribution/"+host+"/r/dist:1/json", nil)
	req.Header.Set("X-Registry-Auth", good)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var inspect struct {
		Descriptor struct{ Digest, MediaType string }
		Platforms  []struct{ OS, Architecture string }
	}
	json.NewDecoder(res.Body).Decode(&inspect)
	res.Body.Close()
	if res.StatusCode != 200 || inspect.Descriptor.Digest != want.String() || len(inspect.Platforms) != 1 || inspect.Platforms[0].OS != "darwin" {
		t.Fatalf("distribution inspect: %d %+v, want digest %s", res.StatusCode, inspect, want)
	}
	if res := call(t, ts, "GET", "/distribution/"+host+"/r/dist:1/json", "", nil); res.StatusCode != 401 {
		t.Fatalf("distribution inspect without credentials: %d", res.StatusCode)
	}
}

// docker system df reports, in API 1.51's shape, the images with their
// sizes and the containers using them, the volumes with their usage, and
// the build cache.
func TestSystemDF(t *testing.T) {
	ts, e := newEngineServer(t)
	loadImage(t, e, "df/a:1")
	loadImage(t, e, "df/b:1")
	call(t, ts, "POST", "/volumes/create", `{"Name":"dfvol"}`, nil)
	var du struct {
		LayersSize int64
		Images     []struct {
			Size, SharedSize, Containers int64
		}
		Volumes []struct {
			Name      string
			UsageData *struct{ Size, RefCount int64 }
		}
		BuildCache []any
	}
	if res := call(t, ts, "GET", "/system/df", "", &du); res.StatusCode != 200 {
		t.Fatalf("system df: %d", res.StatusCode)
	}
	if len(du.Images) != 2 || du.LayersSize != du.Images[0].Size+du.Images[1].Size || du.Images[0].Containers != 0 {
		t.Fatalf("image usage %+v", du)
	}
	if len(du.Volumes) != 1 || du.Volumes[0].UsageData == nil || du.Volumes[0].UsageData.RefCount != 0 {
		t.Fatalf("volume usage %+v", du.Volumes)
	}
	var images struct {
		Images  []any
		Volumes []any
	}
	call(t, ts, "GET", "/system/df?type=image", "", &images)
	if len(images.Images) != 2 || images.Volumes != nil {
		t.Fatalf("system df for images alone: %+v", images)
	}
	if res := call(t, ts, "GET", "/system/df?type=nonsense", "", nil); res.StatusCode != 400 {
		t.Fatalf("system df for an unknown type: %d", res.StatusCode)
	}
}
