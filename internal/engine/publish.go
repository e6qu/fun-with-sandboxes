package engine

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/moby/moby/api/types/network"
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

// publish binds every requested host port, TCP or UDP. It fails as Docker
// does when a host port is taken, and binds nothing in that case. A host
// port range takes the first port of it the host can bind.
func (e *Engine) publish(c *Container) ([]io.Closer, network.PortMap, error) {
	bindings := network.PortMap{}
	for p, bs := range c.rec.HostConfig.PortBindings {
		bindings[p] = bs
	}
	if c.rec.HostConfig.PublishAllPorts {
		for p := range c.rec.Config.ExposedPorts {
			if _, ok := bindings[p]; !ok && (p.Proto() == network.TCP || p.Proto() == network.UDP) {
				bindings[p] = []network.PortBinding{{}}
			}
		}
	}
	keys := make([]network.Port, 0, len(bindings))
	for p := range bindings {
		keys = append(keys, p)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	var closers []io.Closer
	published := network.PortMap{}
	fail := func(err error) ([]io.Closer, network.PortMap, error) {
		for _, cl := range closers {
			cl.Close()
		}
		return nil, nil, err
	}
	stack := c.rec.Stack
	lease := func() (int, func(), error) {
		port, err := e.alloc.Lease(stack)
		if err != nil {
			return 0, nil, err
		}
		return port, func() { e.alloc.Unlease(stack, port) }, nil
	}
	for _, p := range keys {
		target, ok := c.rec.Ports[p.String()]
		if !ok {
			continue
		}
		upstream := net.JoinHostPort("127.0.0.1", strconv.Itoa(target))
		for _, b := range bindings[p] {
			ip := b.HostIP
			if !ip.IsValid() {
				ip = netip.IPv4Unspecified()
			}
			lo, hi, err := hostPortRange(b.HostPort)
			if err != nil {
				return fail(invalid("invalid host port %q: %v", b.HostPort, err))
			}
			cl, port, err := listenRange(p.Proto(), ip, lo, hi, upstream, lease)
			if err != nil {
				if errors.Is(err, syscall.EADDRINUSE) {
					return fail(conflict("Bind for %s:%s failed: port is already allocated", ip, portText(lo, hi)))
				}
				return fail(err)
			}
			closers = append(closers, cl)
			published[p] = append(published[p], network.PortBinding{HostIP: ip, HostPort: strconv.Itoa(port)})
		}
	}
	return closers, published, nil
}

// hostPortRange reads a binding's host port: empty for any port, a port, or
// a range low-high.
func hostPortRange(s string) (int, int, error) {
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

func portText(lo, hi int) string {
	if lo == hi {
		return strconv.Itoa(lo)
	}
	return fmt.Sprintf("%d-%d", lo, hi)
}

// leaser lends a port of the container's stack block and returns a func that
// gives it back.
type leaser func() (int, func(), error)

// listenRange binds the first port from lo to hi the host lets it, and
// forwards what arrives there to upstream; UDP flows send from ports lease
// lends.
func listenRange(proto network.IPProtocol, ip netip.Addr, lo, hi int, upstream string, lease leaser) (io.Closer, int, error) {
	var err error
	for port := lo; port <= hi; port++ {
		addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
		if proto == network.UDP {
			var pc net.PacketConn
			if pc, err = net.ListenPacket("udp", addr); err == nil {
				px := newUDPProxy(pc, upstream, lease)
				go px.serve()
				return px, pc.LocalAddr().(*net.UDPAddr).Port, nil
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

// udpIdleTimeout is how long a UDP flow lasts without traffic, as Docker's
// userland proxy keeps its flows.
const udpIdleTimeout = 90 * time.Second

// udpProxy forwards datagrams between a published UDP port and the
// container's loopback port, keeping one upstream socket per client so
// replies reach the client that asked. The container's profile lets it send
// only to its stack's ports, so each upstream socket binds a port of the
// stack's block, leased for the flow's life.
type udpProxy struct {
	pc       net.PacketConn
	upstream string
	lease    leaser
	mu       sync.Mutex
	flows    map[string]net.Conn
	closed   bool
	wg       sync.WaitGroup
}

func newUDPProxy(pc net.PacketConn, upstream string, lease leaser) *udpProxy {
	return &udpProxy{pc: pc, upstream: upstream, lease: lease, flows: map[string]net.Conn{}}
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
	raddr, err := net.ResolveUDPAddr("udp", p.upstream)
	if err != nil {
		return nil, err
	}
	port, unlease, err := p.lease()
	if err != nil {
		return nil, err
	}
	up, err := net.DialUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}, raddr)
	if err != nil {
		unlease()
		return nil, err
	}
	p.flows[client.String()] = up
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer unlease()
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
