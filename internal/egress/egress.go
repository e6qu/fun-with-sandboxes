// Package egress is the proxy through which a stack's containers reach the
// internet: an HTTP proxy, and a SOCKS5 one on the same port for any TCP.
// Seatbelt can only allow loopback ports or everything, so the containers may
// connect to this proxy alone, and the proxy refuses every destination on the
// host itself.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"
)

// Policy decides which addresses the proxy may dial.
type Policy struct {
	// Deny reports whether ip must not be dialled.
	Deny func(ip net.IP) bool
	// Resolve looks up a host name; it defaults to the system resolver.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
}

// HostPolicy refuses loopback, link-local, unspecified and multicast
// addresses, and every address assigned to one of the host's interfaces:
// those all reach the host, not the internet.
func HostPolicy() Policy {
	return Policy{Deny: func(ip net.IP) bool { return IsHostAddress(ip, interfaceAddrs()) }}
}

// IsHostAddress reports whether ip reaches this machine.
func IsHostAddress(ip net.IP, own []net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, o := range own {
		if o.Equal(ip) {
			return true
		}
	}
	return false
}

func interfaceAddrs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out
}

// ErrDenied is returned for a destination the policy refuses.
var ErrDenied = errors.New("egress: destination is on the host")

type Proxy struct {
	l      net.Listener
	srv    *http.Server
	policy Policy
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// Listen starts a proxy on addr.
func Listen(addr string, policy Policy) (*Proxy, error) {
	if policy.Resolve == nil {
		policy.Resolve = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			ips := make([]net.IP, len(addrs))
			for i, a := range addrs {
				ips[i] = a.IP
			}
			return ips, nil
		}
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	p := &Proxy{l: l, policy: policy, conns: map[net.Conn]struct{}{}}
	forward := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL = r.In.URL
			r.Out.Host = r.In.Host
		},
		Transport: &http.Transport{DialContext: p.dial, ForceAttemptHTTP2: false, ResponseHeaderTimeout: 5 * time.Minute},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.fail(w, err)
		},
	}
	p.srv = &http.Server{
		ReadHeaderTimeout: 30 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodConnect:
				p.connect(w, r)
			case r.URL.IsAbs() && r.URL.Scheme == "http":
				forward.ServeHTTP(w, r)
			default:
				http.Error(w, "egress proxy: send absolute http:// URLs or CONNECT", http.StatusBadRequest)
			}
		}),
		ConnState: func(c net.Conn, s http.ConnState) {
			switch s {
			case http.StateNew:
				p.track(c)
			case http.StateClosed:
				p.untrack(c)
			}
		},
	}
	go p.srv.Serve(newSniffer(l, p))
	return p, nil
}

// Variables are the environment variables that send a process's traffic
// through the proxy on port: HTTP and HTTPS through its HTTP side, other
// TCP, for software that honours ALL_PROXY, through its SOCKS5 side, which
// resolves names itself (socks5h), and loopback around it.
func Variables(port int) [][2]string {
	web := fmt.Sprintf("http://127.0.0.1:%d", port)
	socks := fmt.Sprintf("socks5h://127.0.0.1:%d", port)
	return [][2]string{
		{"HTTP_PROXY", web}, {"HTTPS_PROXY", web}, {"http_proxy", web}, {"https_proxy", web},
		{"ALL_PROXY", socks}, {"all_proxy", socks},
		{"NO_PROXY", "localhost,127.0.0.1,::1"}, {"no_proxy", "localhost,127.0.0.1,::1"},
	}
}

func (p *Proxy) Addr() *net.TCPAddr { return p.l.Addr().(*net.TCPAddr) }

func (p *Proxy) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrDenied) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	http.Error(w, "egress proxy: "+err.Error(), http.StatusBadGateway)
}

// dial resolves addr once, checks every address against the policy, and
// dials an address it checked, so a name cannot resolve to the host between
// the check and the connection.
func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		ips = []net.IP{ip}
	} else if ips, err = p.policy.Resolve(ctx, host); err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("egress: %s has no addresses", host)
	}
	for _, ip := range ips {
		if p.policy.Deny(ip) {
			return nil, fmt.Errorf("%w: %s resolves to %s", ErrDenied, host, ip)
		}
	}
	var d net.Dialer
	var last error
	for _, ip := range ips {
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	up, err := p.dial(r.Context(), "tcp", r.Host)
	if err != nil {
		p.fail(w, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "egress proxy: cannot hijack", http.StatusInternalServerError)
		return
	}
	client, rw, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if !p.track(up) {
		up.Close()
		client.Close()
		return
	}
	defer p.untrack(up)
	defer client.Close()
	io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	// Bytes the client sent after its request line are already buffered.
	if n := rw.Reader.Buffered(); n > 0 {
		b, _ := rw.Reader.Peek(n)
		up.Write(b)
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, client); closeWrite(up); done <- struct{}{} }()
	go func() { io.Copy(client, up); closeWrite(client); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
		return
	}
	c.Close()
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	c.Close()
}

// Close stops accepting and severs every open connection.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	return p.srv.Close()
}
