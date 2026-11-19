package containerd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The configuration the node starts with asks nothing of the engine.
func TestTheNodesOwnConfigurationAsksNothing(t *testing.T) {
	s, err := Parse([]byte(Config))
	if err != nil || len(s.Mirrors) != 0 || s.HostsDir != "" || len(s.Unsupported) != 0 {
		t.Errorf("Parse(Config) = %+v, %v", s, err)
	}
}

// kind's local registry patches, the hosts directory and a mirror, are
// taken; any other setting is listed as one the engine does not apply.
func TestPatches(t *testing.T) {
	patched := Config + `
[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d"
[plugins."io.containerd.grpc.v1.cri".registry.mirrors."localhost:5001"]
  endpoint = ["http://kind-registry:5000", "registry.example:443"]
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  SystemdCgroup = true
`
	s, err := Parse([]byte(patched))
	if err != nil {
		t.Fatal(err)
	}
	if s.HostsDir != "/etc/containerd/certs.d" {
		t.Errorf("hosts directory %q", s.HostsDir)
	}
	want := []Endpoint{{Host: "kind-registry:5000", Insecure: true}, {Host: "registry.example:443"}}
	if got := s.Mirrors["localhost:5001"]; !reflect.DeepEqual(got, want) {
		t.Errorf("mirrors %+v, want %+v", got, want)
	}
	if want := []string{`plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options.SystemdCgroup`}; !reflect.DeepEqual(s.Unsupported, want) {
		t.Errorf("unsupported %q, want %q", s.Unsupported, want)
	}
	// A changed snapshotter is a setting the engine does not apply.
	s, err = Parse([]byte(`version = 2
[plugins."io.containerd.grpc.v1.cri".containerd]
  snapshotter = "overlayfs"
`))
	if err != nil || len(s.Unsupported) != 1 {
		t.Errorf("a changed snapshotter: %+v, %v", s, err)
	}
	if _, err := Parse([]byte("not = [toml")); err == nil {
		t.Error("a configuration that is not TOML parsed")
	}
}

// A registry's hosts.toml names its hosts in order, those that pull, then
// its server; _default's stands for a registry without one.
func TestHosts(t *testing.T) {
	dir := t.TempDir()
	write := func(reg, text string) {
		if err := os.MkdirAll(filepath.Join(dir, reg), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, reg, "hosts.toml"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("localhost:5001", `server = "https://upstream.example"

[host."http://kind-registry:5000"]
  capabilities = ["pull", "resolve"]

[host."https://push-only.example"]
  capabilities = ["push"]

[host."https://mirror.example"]
  skip_verify = true
`)
	write("_default", `[host."http://fallback:5000"]`+"\n")
	got, err := Hosts(os.ReadFile, dir, "localhost:5001")
	want := []Endpoint{{Host: "kind-registry:5000", Insecure: true}, {Host: "mirror.example", SkipVerify: true}, {Host: "upstream.example"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Hosts(localhost:5001) = %+v, %v; want %+v", got, err, want)
	}
	if got, err := Hosts(os.ReadFile, dir, "docker.io"); err != nil || !reflect.DeepEqual(got, []Endpoint{{Host: "fallback:5000", Insecure: true}}) {
		t.Errorf("Hosts(docker.io) = %+v, %v; want _default's", got, err)
	}
	if got, err := Hosts(os.ReadFile, t.TempDir(), "docker.io"); err != nil || got != nil {
		t.Errorf("Hosts in an empty directory = %+v, %v", got, err)
	}
}
