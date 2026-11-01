package engine

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"syscall"

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

// publish binds every requested host port. It fails as Docker does when a
// host port is taken, and binds nothing in that case.
func (e *Engine) publish(c *Container) ([]io.Closer, network.PortMap, error) {
	bindings := network.PortMap{}
	for p, bs := range c.rec.HostConfig.PortBindings {
		bindings[p] = bs
	}
	if c.rec.HostConfig.PublishAllPorts {
		for p := range c.rec.Config.ExposedPorts {
			if _, ok := bindings[p]; !ok && p.Proto() == network.TCP {
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
	for _, p := range keys {
		target, ok := c.rec.Ports[p.String()]
		if !ok {
			continue
		}
		for _, b := range bindings[p] {
			ip := b.HostIP
			if !ip.IsValid() {
				ip = netip.IPv4Unspecified()
			}
			port := b.HostPort
			if port == "" {
				port = "0"
			}
			l, err := net.Listen("tcp", net.JoinHostPort(ip.String(), port))
			if err != nil {
				if errors.Is(err, syscall.EADDRINUSE) {
					return fail(conflict("Bind for %s:%s failed: port is already allocated", ip, port))
				}
				return fail(err)
			}
			px := &proxy{l: l, target: net.JoinHostPort("127.0.0.1", strconv.Itoa(target)), conns: map[net.Conn]struct{}{}}
			go px.serve()
			closers = append(closers, px)
			actual := l.Addr().(*net.TCPAddr)
			published[p] = append(published[p], network.PortBinding{HostIP: ip, HostPort: strconv.Itoa(actual.Port)})
		}
	}
	return closers, published, nil
}
