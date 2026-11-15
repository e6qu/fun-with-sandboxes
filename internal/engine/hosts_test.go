package engine

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

func TestWriteHostsFiles(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	outside := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(outside, []byte("host's own\n"), 0o644)
	// An image whose /etc/hosts is a symlink out of the root.
	os.Symlink(outside, filepath.Join(root, "etc", "hosts"))
	if err := writeHostsFiles(root, "web-1", "", []string{"127.88.0.4", "fd00:5::3"}, []string{"db.local:10.0.0.5", "host.docker.internal:host-gateway"}, []string{"127.88.0.2\tdb db-host other"}, dnsSettings{}, nil); err != nil {
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
	if err := writeHostsFiles(root2, "x", "", nil, nil, nil, dnsSettings{}, []mountRec{{Destination: "/etc/hosts"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root2, "etc", "hosts")); !os.IsNotExist(err) {
		t.Errorf("wrote /etc/hosts over a mount: %v", err)
	}
	// A domain name makes the container's name a fully qualified one.
	root3 := t.TempDir()
	if err := writeHostsFiles(root3, "web", "example.com", []string{"127.88.0.4"}, nil, nil, dnsSettings{}, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root3, "etc", "hosts")); !strings.HasSuffix(string(b), "127.88.0.4\tweb.example.com web\n") {
		t.Errorf("/etc/hosts with a domain name:\n%s", b)
	}
	if err := writeHostsFiles(t.TempDir(), "x", "", nil, []string{"bad"}, nil, dnsSettings{}, nil); err == nil {
		t.Error("accepted --add-host without an address")
	}
}

// A container's resolv.conf is the host's without its loopback nameservers,
// with Google's public ones when none remain, as Docker writes it.
func TestResolvConf(t *testing.T) {
	for _, c := range []struct{ host, want string }{
		{"# comment\nnameserver 192.168.1.1\nnameserver 127.0.0.53\nsearch lan\noptions ndots:2\n", "# comment\nnameserver 192.168.1.1\nsearch lan\noptions ndots:2\n"},
		{"nameserver 127.0.0.1\nnameserver ::1\nsearch lan\n", "search lan\nnameserver 8.8.8.8\nnameserver 8.8.4.4\n"},
		{"", "nameserver 8.8.8.8\nnameserver 8.8.4.4\n"},
	} {
		if got := resolvConf([]byte(c.host), dnsSettings{}); got != c.want {
			t.Errorf("resolvConf(%q) = %q, want %q", c.host, got, c.want)
		}
	}
	root := t.TempDir()
	if err := writeHostsFiles(root, "x", "", nil, nil, nil, dnsSettings{}, nil); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "etc", "resolv.conf")); err != nil || !strings.Contains(string(b), "nameserver ") {
		t.Errorf("/etc/resolv.conf: %q, %v", b, err)
	}
}

// A container's own DNS servers, search domains and options replace the
// host's, as Docker writes them; a search domain of "." alone drops the
// host's.
func TestResolvConfWithDNSSettings(t *testing.T) {
	host := "# comment\nnameserver 192.168.1.1\nsearch lan\noptions ndots:2\n"
	one, two := netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2606:4700::1111")
	for _, c := range []struct {
		d    dnsSettings
		want string
	}{
		{dnsSettings{Servers: []netip.Addr{one, two}}, "# comment\nsearch lan\noptions ndots:2\nnameserver 1.1.1.1\nnameserver 2606:4700::1111\n"},
		{dnsSettings{Search: []string{"a.example", "b.example"}}, "# comment\nnameserver 192.168.1.1\noptions ndots:2\nsearch a.example b.example\n"},
		{dnsSettings{Search: []string{"."}}, "# comment\nnameserver 192.168.1.1\noptions ndots:2\n"},
		{dnsSettings{Options: []string{"ndots:5", "timeout:1"}}, "# comment\nnameserver 192.168.1.1\nsearch lan\noptions ndots:5 timeout:1\n"},
	} {
		if got := resolvConf([]byte(host), c.d); got != c.want {
			t.Errorf("resolvConf(%+v) = %q, want %q", c.d, got, c.want)
		}
	}
	root := t.TempDir()
	if err := writeHostsFiles(root, "x", "", nil, nil, nil, dnsSettings{Servers: []netip.Addr{one}, Search: []string{"corp.example"}}, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "etc", "resolv.conf"))
	d := router.ParseResolvConf(b)
	if len(d.Servers) != 1 || d.Servers[0] != one || len(d.Search) != 1 || d.Search[0] != "corp.example" {
		t.Errorf("/etc/resolv.conf %q reads as %+v", b, d)
	}
}

func TestCheckDNS(t *testing.T) {
	for _, c := range []struct {
		d    dnsSettings
		kind Kind
	}{
		{dnsSettings{Servers: []netip.Addr{netip.MustParseAddr("1.1.1.1")}, Search: []string{"example.com", "."}, Options: []string{"ndots:2", "rotate"}}, -1},
		{dnsSettings{Servers: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}, KindUnsupported},
		{dnsSettings{Servers: []netip.Addr{netip.MustParseAddr("127.88.0.3")}}, KindUnsupported},
		{dnsSettings{Servers: []netip.Addr{netip.MustParseAddr("::1")}}, KindUnsupported},
		{dnsSettings{Search: []string{"a b"}}, KindInvalid},
		{dnsSettings{Options: []string{""}}, KindInvalid},
	} {
		err := checkDNS(c.d)
		if c.kind < 0 && err != nil || c.kind >= 0 && (err == nil || KindOf(err) != c.kind) {
			t.Errorf("checkDNS(%+v) = %v, want kind %d", c.d, err, c.kind)
		}
	}
}
