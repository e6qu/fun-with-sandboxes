package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/net/proxy"

	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// testPolicy treats 127.0.0.1 as the internet and every other address as the
// host, so a loopback server can stand in for a remote one.
func testPolicy() Policy {
	return Policy{
		Deny: func(ip net.IP) bool { return !ip.Equal(net.IPv4(127, 0, 0, 1)) },
		Resolve: func(_ context.Context, host string) ([]net.IP, error) {
			switch host {
			case "remote.test":
				return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
			case "rebind.test":
				return []net.IP{net.IPv4(127, 0, 0, 1), net.IPv4(10, 0, 0, 1)}, nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		},
	}
}

func startProxy(t *testing.T) *Proxy {
	t.Helper()
	p, err := Listen("127.0.0.1:0", testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func client(p *Proxy) *http.Client {
	u, _ := url.Parse("http://" + p.Addr().String())
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
}

func TestForwardsAllowedHTTP(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello "+r.Host) }))
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	res, err := client(startProxy(t)).Get("http://remote.test:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(b) != "hello remote.test:"+port {
		t.Fatalf("got %d %q", res.StatusCode, b)
	}
}

func TestTunnelsAllowedCONNECT(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure") }))
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	c := client(startProxy(t))
	c.Transport.(*http.Transport).TLSClientConfig = up.Client().Transport.(*http.Transport).TLSClientConfig
	res, err := c.Get("https://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if b, _ := io.ReadAll(res.Body); string(b) != "secure" {
		t.Fatalf("got %q", b)
	}
}

func TestRefusesHostDestinations(t *testing.T) {
	p := startProxy(t)
	for _, target := range []string{"127.0.0.2:80", "rebind.test:80", "[::1]:80"} {
		conn, err := net.Dial("tcp", p.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("CONNECT %s: %d, want 403", target, res.StatusCode)
		}
	}
	res, err := client(p).Get("http://127.0.0.2/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("GET via proxy to 127.0.0.2: %d, want 403", res.StatusCode)
	}
}

func TestRejectsOriginFormRequests(t *testing.T) {
	p := startProxy(t)
	res, err := http.Get("http://" + p.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("direct request: %d, want 400", res.StatusCode)
	}
}

func TestIsHostAddress(t *testing.T) {
	own := []net.IP{net.ParseIP("192.168.1.20")}
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "0.0.0.0": true, "169.254.169.254": true, "fe80::1": true,
		"224.0.0.1": true, "192.168.1.20": true, "192.168.1.21": false, "140.82.112.3": false, "2606:4700::1111": false,
	} {
		if got := IsHostAddress(net.ParseIP(ip), own); got != want {
			t.Errorf("IsHostAddress(%s) = %v, want %v", ip, got, want)
		}
	}
}

func memberClient(p *Proxy, member, password string) *http.Client {
	u := &url.URL{Scheme: "http", User: url.UserPassword(member, password), Host: p.Addr().String()}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
}

// A stack's members are reached at the host ports behind their ports, by
// name or member address, though the policy denies loopback: a client that
// names itself a member, and sends the stack's own traffic through the
// proxy, reaches the members it sees.
func TestReachesStackMembers(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "member "+r.Host) }))
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	network := filepath.Join(t.TempDir(), "network")
	// The member's port 9000 has no host port: its router reaches it.
	dir, err := os.MkdirTemp("/tmp", "fwse")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ul, err := net.Listen("unix", router.SocketPath(dir, 9000))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	go http.Serve(ul, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "member routed") }))
	rt, err := router.Listen("127.0.0.1:0", dir, func() (*net.UDPConn, error) {
		return net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	}, router.Network{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	os.WriteFile(network, []byte(fmt.Sprintf(
		"127.88.0.2\tweb proj-web-1\t8000/tcp=%s\t%d\tproj_default\t0\t\t127.88.0.3/front\tproj_default=10.9.0.2/24 proj_default=fd00:9::2/64\n"+
			"127.88.0.3\tapp\t\t0\tproj_default\t0\t\t\tproj_default=10.9.0.3/24\n"+
			"127.88.0.4\tdb\t5432/tcp=%[1]s\t0\tproj_back\t0\n", port, rt.Addr().Port)), 0o644)
	policy := testPolicy()
	policy.Deny = func(net.IP) bool { return true }
	policy.Members = network
	policy.Key = []byte("stack key")
	p, err := Listen("127.0.0.1:0", policy)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	app := memberClient(p, "127.88.0.3", Password(policy.Key, "127.88.0.3"))
	get := func(c *http.Client, url string) (int, string) {
		t.Helper()
		res, err := c.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	for _, url := range []string{"http://web:8000/", "http://PROJ-WEB-1:8000/", "http://127.88.0.2:8000/", "http://10.9.0.2:8000/", "http://[fd00:9::2]:8000/", "http://web:9000/", "http://front:8000/"} {
		if code, b := get(app, url); code != 200 || !strings.HasPrefix(b, "member ") {
			t.Errorf("GET %s: %d %q", url, code, b)
		}
	}
	if code, _ := get(app, "http://web:9999/"); code != http.StatusBadGateway {
		t.Errorf("a port the member does not expose: %d, want 502", code)
	}
	// An address of the network's subnet no member holds reaches nothing,
	// for a client that names no member too.
	for _, c := range []*http.Client{app, client(p)} {
		for _, url := range []string{"http://10.9.0.77:8000/", "http://[fd00:9::77]:8000/"} {
			if code, b := get(c, url); code != http.StatusBadGateway || !strings.Contains(b, syscall.EHOSTUNREACH.Error()) {
				t.Errorf("%s, an address of a subnet no member holds: %d %q", url, code, b)
			}
		}
	}
	// A member on no network the client is on is no member to it, and a
	// client that names no member reaches none.
	for _, url := range []string{"http://db:5432/", "http://127.88.0.4:5432/"} {
		if code, _ := get(app, url); code == 200 {
			t.Errorf("GET %s from a member on another network: %d", url, code)
		}
	}
	web := memberClient(p, "127.88.0.2", Password(policy.Key, "127.88.0.2"))
	if code, _ := get(web, "http://front:8000/"); code == 200 {
		t.Error("a link's alias resolved for a member that does not make the link")
	}
	for _, url := range []string{"http://web:8000/", "http://127.88.0.2:8000/"} {
		if code, _ := get(client(p), url); code == 200 {
			t.Errorf("GET %s from a client that names no member: %d", url, code)
		}
	}
	for name, c := range map[string]*http.Client{
		"a wrong password":         memberClient(p, "127.88.0.3", Password(policy.Key, "127.88.0.2")),
		"another stack's password": memberClient(p, "127.88.0.3", Password([]byte("other key"), "127.88.0.3")),
	} {
		if code, _ := get(c, "http://web:8000/"); code != http.StatusProxyAuthRequired {
			t.Errorf("%s: %d, want 407", name, code)
		}
	}
	// The SOCKS5 side takes the same credentials (RFC 1929).
	for user, want := range map[string]bool{"127.88.0.3": true, "127.88.0.4": false} {
		d, err := proxy.SOCKS5("tcp", p.Addr().String(), &proxy.Auth{User: user, Password: Password(policy.Key, user)}, proxy.Direct)
		if err != nil {
			t.Fatal(err)
		}
		c, err := d.Dial("tcp", "web:8000")
		if err == nil {
			c.Close()
		}
		if (err == nil) != want {
			t.Errorf("SOCKS5 to web:8000 as %s: %v", user, err)
		}
	}
	d, _ := proxy.SOCKS5("tcp", p.Addr().String(), &proxy.Auth{User: "127.88.0.3", Password: "guess"}, proxy.Direct)
	if c, err := d.Dial("tcp", "web:8000"); err == nil {
		c.Close()
		t.Error("SOCKS5 took a wrong password")
	}
}
