package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
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
