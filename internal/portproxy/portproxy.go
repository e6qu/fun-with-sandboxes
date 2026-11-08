// Package portproxy forwards a published host port to the loopback port a
// container listens on, TCP or UDP, as Docker's userland proxy does.
package portproxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// proxy accepts on a published host port and forwards each connection to the
// loopback port the container listens on.
type proxy struct {
	l      net.Listener
	target string
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

func (p *proxy) serve() {
	for {
		c, err := p.l.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			c.Close()
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.untrack(c)
			up, err := net.Dial("tcp", p.target)
			if err != nil {
				c.Close()
				return
			}
			p.track(up)
			defer p.untrack(up)
			pipe(c, up)
		}()
	}
}

func (p *proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	c.Close()
}

func (p *proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	err := p.l.Close()
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
	return err
}

func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	half := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go half(a, b)
	go half(b, a)
	wg.Wait()
}

// HostPortRange reads a binding's host port: empty for any port, a port, or
// a range low-high.
func HostPortRange(s string) (int, int, error) {
	if s == "" {
		return 0, 0, nil
	}
	a, b, isRange := strings.Cut(s, "-")
	lo, err := strconv.ParseUint(a, 10, 16)
	if err != nil {
		return 0, 0, err
	}
	hi := lo
	if isRange {
		if hi, err = strconv.ParseUint(b, 10, 16); err != nil {
			return 0, 0, err
		}
		if hi < lo {
			return 0, 0, fmt.Errorf("range %s ends below its start", s)
		}
	}
	return int(lo), int(hi), nil
}

// PortText renders a host port or range as Docker's errors do.
func PortText(lo, hi int) string {
	if lo == hi {
		return strconv.Itoa(lo)
	}
	return fmt.Sprintf("%d-%d", lo, hi)
}

// FlowDialer opens the upstream socket of a UDP flow to raddr.
type FlowDialer func(raddr *net.UDPAddr) (net.Conn, error)

// Listen binds the first port from lo to hi the host lets it, TCP or UDP
// by proto, and forwards what arrives there to upstream; UDP flows send from
// the sockets dial opens.
func Listen(proto string, ip netip.Addr, lo, hi int, upstream string, dial FlowDialer) (io.Closer, int, error) {
	var err error
	for port := lo; port <= hi; port++ {
		addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
		if proto == "udp" {
			var pc net.PacketConn
			if pc, err = net.ListenPacket("udp", addr); err == nil {
				return ServeUDP(pc, upstream, dial, nil), pc.LocalAddr().(*net.UDPAddr).Port, nil
			}
		} else {
			var l net.Listener
			if l, err = net.Listen("tcp", addr); err == nil {
				px := &proxy{l: l, target: upstream, conns: map[net.Conn]struct{}{}}
				go px.serve()
				return px, l.Addr().(*net.TCPAddr).Port, nil
			}
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, 0, err
		}
	}
	return nil, 0, err
}

// ephemeralLo and ephemeralHi bound the ports ListenUnix picks from when
// it is given none, as Linux picks a published port's host port.
const ephemeralLo, ephemeralHi = 32768, 60999

// ListenUnix listens, for the first port from lo to hi free, or for an
// ephemeral one with lo 0, on the Unix socket path gives that port, and
// forwards each connection to upstream: a TCP port published in a
// container whose own ports without host ports are Unix sockets.
func ListenUnix(path func(port int) string, lo, hi int, upstream string) (io.Closer, int, error) {
	if lo == 0 {
		lo, hi = ephemeralLo, ephemeralHi
	}
	var err error
	for port := lo; port <= hi; port++ {
		var l net.Listener
		if l, err = net.Listen("unix", path(port)); err == nil {
			px := &proxy{l: l, target: upstream, conns: map[net.Conn]struct{}{}}
			go px.serve()
			return px, port, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, 0, err
		}
	}
	return nil, 0, err
}

// udpIdleTimeout is how long a UDP flow lasts without traffic, as Docker's
// userland proxy keeps its flows.
const udpIdleTimeout = 90 * time.Second

var errRefusedClient = errors.New("datagram from a refused client")

// udpProxy forwards datagrams between a published UDP port and the
// container's loopback port, keeping one upstream socket per client so
// replies reach the client that asked. The container's profile lets it send
// only to its stack's ports, so dial binds each upstream socket to a port of
// the stack's block.
type udpProxy struct {
	pc       net.PacketConn
	upstream string
	dial     FlowDialer
	accept   func(*net.UDPAddr) bool
	mu       sync.Mutex
	flows    map[string]net.Conn
	closed   bool
	wg       sync.WaitGroup
}

// ServeUDP forwards the datagrams pc receives to upstream, from a socket
// dial opens for each client, and the replies back to the client. With
// accept, it drops a datagram from a client accept refuses.
func ServeUDP(pc net.PacketConn, upstream string, dial FlowDialer, accept func(*net.UDPAddr) bool) io.Closer {
	px := newUDPProxy(pc, upstream, dial)
	px.accept = accept
	go px.serve()
	return px
}

func newUDPProxy(pc net.PacketConn, upstream string, dial FlowDialer) *udpProxy {
	return &udpProxy{pc: pc, upstream: upstream, dial: dial, flows: map[string]net.Conn{}}
}

func (p *udpProxy) serve() {
	buf := make([]byte, 65535)
	for {
		n, client, err := p.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		up, err := p.flow(client)
		if err != nil {
			continue
		}
		// Traffic either way keeps a flow, as the reader's deadline counts.
		up.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		up.Write(buf[:n])
	}
}

// flow returns the client's upstream socket, opening it and its reply
// reader on the client's first datagram.
func (p *udpProxy) flow(client net.Addr) (net.Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, net.ErrClosed
	}
	if up, ok := p.flows[client.String()]; ok {
		return up, nil
	}
	if ua, ok := client.(*net.UDPAddr); p.accept != nil && (!ok || !p.accept(ua)) {
		return nil, errRefusedClient
	}
	raddr, err := net.ResolveUDPAddr("udp", p.upstream)
	if err != nil {
		return nil, err
	}
	up, err := p.dial(raddr)
	if err != nil {
		return nil, err
	}
	p.flows[client.String()] = up
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.drop(client.String(), up)
		buf := make([]byte, 65535)
		for {
			up.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			n, err := up.Read(buf)
			if err != nil {
				return
			}
			if _, err := p.pc.WriteTo(buf[:n], client); err != nil {
				return
			}
		}
	}()
	return up, nil
}

func (p *udpProxy) drop(key string, up net.Conn) {
	p.mu.Lock()
	if p.flows[key] == up {
		delete(p.flows, key)
	}
	p.mu.Unlock()
	up.Close()
}

func (p *udpProxy) Close() error {
	p.mu.Lock()
	p.closed = true
	err := p.pc.Close()
	for _, up := range p.flows {
		up.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
	return err
}
