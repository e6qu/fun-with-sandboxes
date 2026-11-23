package egress

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseAllow(t *testing.T) {
	for _, s := range []string{"example.com", "*.example.com:443, 10.0.0.0/8", "1.2.3.4:80,[2001:db8::1]:443", "Example.COM."} {
		if _, err := ParseAllow(s); err != nil {
			t.Errorf("ParseAllow(%q): %v", s, err)
		}
	}
	for _, s := range []string{"", " , ", "a:b", "example.com:0", "*", "foo*.com", "10.0.0.0/33"} {
		if _, err := ParseAllow(s); err == nil {
			t.Errorf("ParseAllow(%q) accepted", s)
		}
	}
}

func TestAllows(t *testing.T) {
	rules, err := ParseAllow("example.com:443, *.svc.test, 10.1.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	ip := func(s string) []net.IP { return []net.IP{net.ParseIP(s)} }
	for _, c := range []struct {
		host string
		ips  []net.IP
		port int
		want bool
	}{
		{"example.com", ip("93.184.0.1"), 443, true},
		{"EXAMPLE.com.", ip("93.184.0.1"), 443, true},
		{"example.com", ip("93.184.0.1"), 80, false},
		{"api.svc.test", ip("8.8.8.8"), 1234, true},
		{"svc.test", ip("8.8.8.8"), 1234, false},
		{"other.test", ip("10.1.2.3"), 22, true},
		{"other.test", []net.IP{net.ParseIP("10.1.2.3"), net.ParseIP("10.2.0.1")}, 22, false},
		{"10.1.9.9", ip("10.1.9.9"), 22, true},
		{"evil.test", ip("1.1.1.1"), 443, false},
	} {
		if got := allows(rules, c.host, c.ips, c.port); got != c.want {
			t.Errorf("allows(%s %v :%d) = %v, want %v", c.host, c.ips, c.port, got, c.want)
		}
	}
}

// A member with an allow list reaches the hosts on it alone through the
// proxy; one without reaches any.
func TestProxyHoldsAMemberToItsAllowList(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	network := filepath.Join(t.TempDir(), "network")
	os.WriteFile(network, []byte("127.88.0.2\tlimited\t\t0\tproj_default\t0\n127.88.0.3\tfree\t\t0\tproj_default\t0\n"), 0o644)
	os.WriteFile(AllowPath(network), []byte(`{"127.88.0.2":["remote.test:`+port+`"]}`), 0o644)
	policy := testPolicy()
	policy.Members = network
	policy.Key = []byte("stack key")
	p, err := Listen("127.0.0.1:0", policy)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	get := func(member, host string) (int, error) {
		res, err := memberClient(p, member, Password(policy.Key, member)).Get("http://" + net.JoinHostPort(host, port))
		if err != nil {
			return 0, err
		}
		res.Body.Close()
		return res.StatusCode, nil
	}
	if code, err := get("127.88.0.2", "remote.test"); err != nil || code != 200 {
		t.Errorf("a host on the list: %d, %v", code, err)
	}
	if code, err := get("127.88.0.2", "127.0.0.1"); err == nil && code == 200 {
		t.Errorf("an address off the list was reached")
	}
	if code, err := get("127.88.0.3", "127.0.0.1"); err != nil || code != 200 {
		t.Errorf("a member without a list: %d, %v", code, err)
	}
}
