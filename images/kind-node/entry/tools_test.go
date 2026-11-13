package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeEngine serves the engine's image API on a Unix socket and records
// the requests it gets.
func fakeEngine(t *testing.T) (engineAPI, func() []string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "engine.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.String()+" "+string(b))
		mu.Unlock()
		if r.URL.Path == "/images/json" {
			io.WriteString(w, `[{"Id":"sha256:abc","RepoTags":["fws/nginx:1"],"Size":7}]`)
			return
		}
		io.WriteString(w, `{"stream":"Loaded image: fws/nginx:1\n"}`)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return engineClient(sock), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// kind's commands in a node, as nodeutils runs them.
func TestCtr(t *testing.T) {
	api, requests := fakeEngine(t)
	var out bytes.Buffer
	if code := ctr(api, []string{"--namespace=k8s.io", "images", "import", "--all-platforms", "--digests", "--snapshotter=native", "-"}, strings.NewReader("archive"), &out); code != 0 {
		t.Fatalf("import: %d", code)
	}
	if code := ctr(api, []string{"--namespace=k8s.io", "images", "tag", "--force", "sha256:abc", "docker.io/fws/nginx:1"}, nil, &out); code != 0 {
		t.Fatalf("tag: %d", code)
	}
	if code := ctr(api, []string{"-n", "k8s.io", "i", "ls"}, nil, &out); code != 0 || !strings.Contains(out.String(), "fws/nginx:1\tsha256:abc\t7") {
		t.Fatalf("ls: %d %q", code, out.String())
	}
	want := []string{
		"POST /images/load?quiet=1 archive",
		"POST /images/sha256:abc/tag?repo=docker.io%2Ffws%2Fnginx&tag=1 ",
		"GET /images/json ",
	}
	if got := requests(); !reflect.DeepEqual(got, want) {
		t.Errorf("requests %q, want %q", got, want)
	}
	if code := ctr(api, []string{"containers", "ls"}, nil, &out); code == 0 {
		t.Error("ctr containers ls succeeded")
	}
}

func TestContainerdConfigDump(t *testing.T) {
	var out bytes.Buffer
	if code := containerd([]string{"config", "dump"}, &out); code != 0 {
		t.Fatalf("config dump: %d", code)
	}
	if s := out.String(); !strings.HasPrefix(s, "version = 2\n") || !strings.Contains(s, `[plugins."io.containerd.grpc.v1.cri".containerd]`+"\n  snapshotter = \"native\"") {
		t.Errorf("config dump: %q", s)
	}
	if code := containerd([]string{"--config", "x"}, &out); code == 0 {
		t.Error("containerd ran")
	}
}

func TestJournalctl(t *testing.T) {
	for unit, want := range map[string]string{
		"":                   "/var/log/fws-kind-node.log",
		"kubelet.service":    "/var/log/kubelet.log",
		"containerd.service": "/var/log/containerd.log",
	} {
		if got := unitLog(unit); got != want {
			t.Errorf("unitLog(%q) = %q, want %q", unit, got, want)
		}
	}
	var out bytes.Buffer
	if code := journalctl([]string{"--no-pager", "-u", "no-such-unit.service"}, &out); code != 0 || out.String() != "-- No entries --\n" {
		t.Errorf("a unit with no log: %d %q", code, out.String())
	}
}

func TestCrictlArgs(t *testing.T) {
	if got := crictlArgs("/run/e.sock", []string{"inspecti", "fws/nginx:1"}); !reflect.DeepEqual(got, []string{"--runtime-endpoint=unix:///run/e.sock", "--image-endpoint=unix:///run/e.sock", "inspecti", "fws/nginx:1"}) {
		t.Errorf("crictlArgs: %q", got)
	}
	if got := crictlArgs("/run/e.sock", []string{"-r", "unix:///x", "ps"}); !reflect.DeepEqual(got, []string{"-r", "unix:///x", "ps"}) {
		t.Errorf("crictlArgs with an endpoint: %q", got)
	}
}
