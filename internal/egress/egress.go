// Package egress is the proxy through which a stack's containers reach the
// internet: an HTTP proxy, and a SOCKS5 one on the same port for any TCP.
// Seatbelt can only allow loopback ports or everything, so the containers may
// connect to this proxy alone, and the proxy refuses every destination on the
// host itself.
package egress

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/members"
	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// Policy decides which addresses the proxy may dial.
type Policy struct {
	// Deny reports whether ip must not be dialled.
	Deny func(ip net.IP) bool
	// Resolve looks up a host name; it defaults to the system resolver.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	// Members names the stack's network file. The proxy reaches a member,
	// by name or member address, at the host port behind its port, as the
	// container library does, for clients that send even the stack's own
	// traffic through the proxy. It reaches a member for a client that
	// names itself, with its credentials, as a member that sees it.
	Members string
	// Key signs the credentials that name a client a member of the stack.
	Key []byte
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

// ErrCredentials is returned for credentials the proxy did not issue.
var ErrCredentials = errors.New("egress: the credentials name no member of the stack")

type Proxy struct {
	l      net.Listener
	srv    *http.Server
	policy Policy
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	// transports keeps a client's idle connections to itself: a name
	// reaches a different member, or none, for another client.
	transports map[string]*http.Transport
	closed     bool
}

// Password is the proxy password of the member at address, which the
// proxy signed with key.
func Password(key []byte, address string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(address))
	return hex.EncodeToString(m.Sum(nil))
}

// identify returns the member the credentials name, "" for none.
func (p *Proxy) identify(user, password string, given bool) (string, error) {
	if !given {
		return "", nil
	}
	if len(p.policy.Key) == 0 || user == "" || !hmac.Equal([]byte(password), []byte(Password(p.policy.Key, user))) {
		return "", ErrCredentials
	}
	return user, nil
}

type clientKey struct{}

func clientOf(ctx context.Context) string {
	s, _ := ctx.Value(clientKey{}).(string)
	return s
}

func (p *Proxy) transport(client string) (*http.Transport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, http.ErrServerClosed
	}
	t, ok := p.transports[client]
	if !ok {
		t = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return p.dial(ctx, client, addr)
			},
			ForceAttemptHTTP2: false, ResponseHeaderTimeout: 5 * time.Minute, IdleConnTimeout: 90 * time.Second,
		}
		p.transports[client] = t
	}
	return t, nil
}

type byClient struct{ p *Proxy }

func (b byClient) RoundTrip(r *http.Request) (*http.Response, error) {
	t, err := b.p.transport(clientOf(r.Context()))
	if err != nil {
		return nil, err
	}
	return t.RoundTrip(r)
}

// proxyAuthorization reads a request's Basic proxy credentials.
func proxyAuthorization(r *http.Request) (user, password string, given bool) {
	h := r.Header.Get("Proxy-Authorization")
	if h == "" {
		return "", "", false
	}
	scheme, enc, _ := strings.Cut(h, " ")
	if !strings.EqualFold(scheme, "Basic") {
		return "", "", true
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return "", "", true
	}
	user, password, _ = strings.Cut(string(b), ":")
	return user, password, true
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
	p := &Proxy{l: l, policy: policy, conns: map[net.Conn]struct{}{}, transports: map[string]*http.Transport{}}
	forward := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL = r.In.URL
			r.Out.Host = r.In.Host
		},
		Transport: byClient{p},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.fail(w, err)
		},
	}
	p.srv = &http.Server{
		ReadHeaderTimeout: 30 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			client, err := p.identify(proxyAuthorization(r))
			if err != nil {
				w.Header().Set("Proxy-Authenticate", `Basic realm="fws egress"`)
				http.Error(w, err.Error(), http.StatusProxyAuthRequired)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), clientKey{}, client))
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
// resolves names itself (socks5h), and loopback and the member block,
// which the container library reaches, around it. A member's process
// names itself to the proxy with the member's address and password.
func Variables(port int, member, password string) [][2]string {
	const noProxy = "localhost,127.0.0.1,::1,127.88.0.0/16"
	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var user *url.Userinfo
	if member != "" {
		user = url.UserPassword(member, password)
	}
	web := (&url.URL{Scheme: "http", User: user, Host: host}).String()
	socks := (&url.URL{Scheme: "socks5h", User: user, Host: host}).String()
	return [][2]string{
		{"HTTP_PROXY", web}, {"HTTPS_PROXY", web}, {"http_proxy", web}, {"https_proxy", web},
		{"ALL_PROXY", socks}, {"all_proxy", socks},
		{"NO_PROXY", noProxy}, {"no_proxy", noProxy},
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
// the check and the connection. A member client reaches the members it
// sees by name and member address.
func (p *Proxy) dial(ctx context.Context, client, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if p.policy.Members != "" && client != "" {
		if c, ok, err := p.member(ctx, client, host, port); ok {
			return c, err
		}
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

// member connects to a port of a member client sees, at the host port
// behind it or through the member's router, and reports whether host is a
// member client sees at all.
func (p *Proxy) member(ctx context.Context, client, host, port string) (net.Conn, bool, error) {
	b, err := os.ReadFile(p.policy.Members)
	if err != nil {
		return nil, false, nil
	}
	m, ok := members.Lookup(members.Parse(b), client, strings.Trim(host, "[]"))
	if !ok {
		return nil, false, nil
	}
	if hp, ok := m.Ports[port+"/tcp"]; ok {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
		return c, true, err
	}
	n, err := strconv.Atoi(port)
	if err != nil || m.Router == 0 {
		return nil, true, fmt.Errorf("egress: %s does not listen on port %s", host, port)
	}
	c, err := router.Dial(net.JoinHostPort("127.0.0.1", strconv.Itoa(m.Router)), n)
	if err != nil {
		return nil, true, fmt.Errorf("egress: %s does not listen on port %s", host, port)
	}
	return c, true, nil
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	up, err := p.dial(r.Context(), clientOf(r.Context()), r.Host)
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
	for _, t := range p.transports {
		t.CloseIdleConnections()
	}
	p.mu.Unlock()
	return p.srv.Close()
}
