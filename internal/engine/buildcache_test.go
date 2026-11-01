package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTar(t *testing.T, path string, mtime time.Time, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for _, name := range []string{"a", "b"} {
		body, ok := files[name]
		if !ok {
			continue
		}
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: mtime, Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
}

// The same change made at another time keys the same; another change does
// not.
func TestContentKeyIgnoresTimes(t *testing.T) {
	dir := t.TempDir()
	writeTar(t, filepath.Join(dir, "1"), time.Unix(1, 0), map[string]string{"a": "x", "b": "y"})
	writeTar(t, filepath.Join(dir, "2"), time.Unix(2, 0), map[string]string{"a": "x", "b": "y"})
	writeTar(t, filepath.Join(dir, "3"), time.Unix(1, 0), map[string]string{"a": "x", "b": "z"})
	k1, _ := contentKey(filepath.Join(dir, "1"))
	k2, _ := contentKey(filepath.Join(dir, "2"))
	k3, _ := contentKey(filepath.Join(dir, "3"))
	if k1 != k2 || k1 == k3 {
		t.Fatalf("content keys %s %s %s: want the first two equal and the third apart", k1, k2, k3)
	}
}

func TestBuildCachePutGetPrune(t *testing.T) {
	c := &buildCache{dir: filepath.Join(t.TempDir(), "cache")}
	if _, ok := c.get("k"); ok {
		t.Fatal("an empty cache returned an entry")
	}
	layer := filepath.Join(t.TempDir(), "layer.tar")
	os.WriteFile(layer, []byte("layer"), 0o600)
	when := time.Unix(1700000000, 0).UTC()
	put, err := c.put("k", when, layer)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.get("k")
	if !ok || !got.Created.Equal(when) || got.Layer != put.Layer {
		t.Fatalf("get after put: %+v, %v", got, ok)
	}
	if b, _ := os.ReadFile(c.layerPath(got)); string(b) != "layer" {
		t.Fatalf("cached layer holds %q", b)
	}
	os.Remove(c.layerPath(got))
	if _, ok := c.get("k"); ok {
		t.Fatal("an entry whose layer is gone was returned")
	}
	if _, err := c.put("m", when, ""); err != nil {
		t.Fatal(err)
	}
	freed, err := c.prune()
	if err != nil || freed == 0 {
		t.Fatalf("prune freed %d, %v", freed, err)
	}
	if _, ok := c.get("m"); ok {
		t.Fatal("prune left an entry")
	}
}

func TestIsGitURL(t *testing.T) {
	for in, want := range map[string]bool{
		"https://github.com/e6qu/x.git":      true,
		"https://example.com/x.git#main:sub": true,
		"git://example.com/x":                true,
		"github.com/e6qu/x":                  true,
		"git@github.com:e6qu/x.git":          true,
		"https://example.com/ctx.tar.gz":     false,
		"https://example.com/Dockerfile":     false,
	} {
		if got := isGitURL(in); got != want {
			t.Errorf("isGitURL(%q) = %v, want %v", in, got, want)
		}
	}
}

// A remote tarball, compressed or not, unpacks as the context, and a remote
// plain file is the Dockerfile.
func TestFetchRemoteTarballAndDockerfile(t *testing.T) {
	var archive bytes.Buffer
	zw := gzip.NewWriter(&archive)
	tw := tar.NewWriter(zw)
	df := []byte("FROM scratch\n")
	tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(df)), Typeflag: tar.TypeReg})
	tw.Write(df)
	tw.Close()
	zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ctx.tar.gz":
			w.Write(archive.Bytes())
		case "/df":
			w.Write([]byte("FROM scratch\nLABEL from=url\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	for path, want := range map[string]string{"/ctx.tar.gz": "FROM scratch\n", "/df": "FROM scratch\nLABEL from=url\n"} {
		dir := filepath.Join(t.TempDir(), "context")
		os.Mkdir(dir, 0o700)
		opts := BuildOptions{Dockerfile: "custom.Dockerfile"}
		if err := fetchRemoteContext(t.Context(), srv.URL+path, dir, &opts); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		name := "Dockerfile"
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != want {
			t.Fatalf("%s: the context's Dockerfile holds %q, %v", path, b, err)
		}
	}
	dir := filepath.Join(t.TempDir(), "context")
	os.Mkdir(dir, 0o700)
	if err := fetchRemoteContext(t.Context(), srv.URL+"/missing", dir, &BuildOptions{}); err == nil {
		t.Fatal("fetching a missing remote context succeeded")
	}
}

// A Git context checks out the fragment's ref and subdirectory from a
// repository served over smart HTTP.
func TestFetchRemoteGitContext(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.MkdirAll(filepath.Join(work, "sub"), 0o755)
	os.WriteFile(filepath.Join(work, "sub", "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	run(work, "init", "--quiet", "-b", "main")
	run(work, "add", ".")
	run(work, "commit", "--quiet", "-m", "first")
	run(work, "tag", "v1")
	os.WriteFile(filepath.Join(work, "sub", "Dockerfile"), []byte("FROM scratch\nLABEL v=2\n"), 0o644)
	run(work, "commit", "--quiet", "-am", "second")
	run(root, "clone", "--quiet", "--bare", work, filepath.Join(root, "srv", "repo.git"))

	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Join(root, "srv"), "GIT_HTTP_EXPORT_ALL=1"},
	}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	for fragment, want := range map[string]string{"#main:sub": "FROM scratch\nLABEL v=2\n", "#v1:sub": "FROM scratch\n"} {
		dir := filepath.Join(t.TempDir(), "context")
		os.Mkdir(dir, 0o700)
		if err := fetchRemoteContext(t.Context(), srv.URL+"/repo.git"+fragment, dir, &BuildOptions{}); err != nil {
			t.Fatalf("%s: %v", fragment, err)
		}
		if b, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err != nil || string(b) != want {
			t.Fatalf("%s: the context's Dockerfile holds %q, %v", fragment, b, err)
		}
	}
}
