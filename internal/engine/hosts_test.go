package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteHostsFiles(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	outside := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(outside, []byte("host's own\n"), 0o644)
	// An image whose /etc/hosts is a symlink out of the root.
	os.Symlink(outside, filepath.Join(root, "etc", "hosts"))
	if err := writeHostsFiles(root, "web-1", []string{"127.88.0.4", "fd00:5::3"}, []string{"db.local:10.0.0.5", "host.docker.internal:host-gateway"}, []string{"127.88.0.2\tdb db-host other"}, nil); err != nil {
		t.Fatal(err)
	}
	hosts, _ := os.ReadFile(filepath.Join(root, "etc", "hosts"))
	want := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n10.0.0.5\tdb.local\n127.0.0.1\thost.docker.internal\n127.88.0.4\tweb-1\nfd00:5::3\tweb-1\n127.88.0.2\tdb db-host other\n"
	if string(hosts) != want {
		t.Errorf("/etc/hosts:\n%s\nwant:\n%s", hosts, want)
	}
	if b, _ := os.ReadFile(outside); string(b) != "host's own\n" {
		t.Errorf("the write followed the image's symlink: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc", "hostname")); string(b) != "web-1\n" {
		t.Errorf("/etc/hostname: %q", b)
	}
	// A mount at /etc/hosts stands.
	root2 := t.TempDir()
	if err := writeHostsFiles(root2, "x", nil, nil, nil, []mountRec{{Destination: "/etc/hosts"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root2, "etc", "hosts")); !os.IsNotExist(err) {
		t.Errorf("wrote /etc/hosts over a mount: %v", err)
	}
	if err := writeHostsFiles(t.TempDir(), "x", nil, []string{"bad"}, nil, nil); err == nil {
		t.Error("accepted --add-host without an address")
	}
}
