// Package router carries traffic to a container's ports that have no host
// port. The container library listens on such a port with a Unix socket in
// the container's port directory, which no other container can reach.
//
// For TCP, a client sends the port it wants to the container's router, a
// loopback port every member of its stack may reach, and the router joins
// the connection to the socket, first writing the client's address there,
// so that the listener's side reports its peer as a TCP server's does.
//
// A client of a member of another stack, on a network the two stacks
// share, reaches it through its own container's router, which relays the
// connection to the member's host port or router when the member shares a
// network with its container. The member's ports lie in its own stack's
// block, which the client's sandbox does not reach. A relay request carries
// the router's relay key, which it keeps in the port directory, so another
// member of the stack cannot ask for one.
//
// For UDP, the router gives each such port a flow port, a loopback UDP port
// of the stack's block, and relays datagrams between it and the port's Unix
// datagram socket. A client asks the router for the flow port once, then
// exchanges datagrams with it as with any host port.
package router

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/members"
	"github.com/e6qu/fun-with-sandboxes/internal/portproxy"
)

// A request is its kind, one byte, then what the kind names; the answer is
// one byte, Connected or Refused. Every port, 0 to 65535, is a port: no
// value of one marks another kind of request.
//
//   - Stream, then the container port, two bytes big-endian: after
//     Connected the connection carries the stream.
//   - StreamRelay, a relay to another stack's member: the relay key, four
//     bytes that name the member's IPv4 address and two its container port.
//   - Flow, a UDP lookup: the UDP port, two bytes; Connected is followed by
//     its flow port, two bytes big-endian.
//   - FlowRelay: a member's UDP port named as StreamRelay names a TCP one,
//     answered with the port of the router's relay to it as Flow is.
const (
	Stream      = 1
	StreamRelay = 2
	Flow        = 3
	FlowRelay   = 4
)

const (
	Connected = 0
	Refused   = 1
)

// KeySize is the size of the relay key.
const KeySize = 16

// KeyPath is where the router keeps its relay key.
func KeyPath(dir string) string { return filepath.Join(dir, "relay-key") }

// Header is the size of what the router puts before a datagram on the Unix
// side: the IPv4 address and port the datagram came from, to the
// container, or goes to, from it.
const Header = 6

// ClientHeader is the size of what the router writes on a connection to a
// container's listener before the client's stream: the IPv4 address and
// port it sees the client at, as Header's are laid out.
const ClientHeader = 6

// ReadClient reads the client's address the router writes before a
// connection's stream.
func ReadClient(r io.Reader) (netip.AddrPort, error) {
	var h [ClientHeader]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte(h[:4])), binary.BigEndian.Uint16(h[4:])), nil
}

// clientHeader is what the router writes for a client at addr.
func clientHeader(addr net.Addr) []byte {
	h := make([]byte, ClientHeader)
	if t, ok := addr.(*net.TCPAddr); ok {
		if v4 := t.IP.To4(); v4 != nil {
			copy(h, v4)
		}
		binary.BigEndian.PutUint16(h[4:], uint16(t.Port))
	}
	return h
}

// SocketPath is where the container library listens on TCP port port.
func SocketPath(dir string, port int) string {
	return filepath.Join(dir, "tcp-"+strconv.Itoa(port))
}

// UDPSocketPath is where the container library receives UDP port port's
// datagrams.
func UDPSocketPath(dir string, port int) string {
	return filepath.Join(dir, "udp-"+strconv.Itoa(port))
}

// UDPRouterPath is where the router receives the datagrams the container's
// UDP sockets on such ports send.
func UDPRouterPath(dir string) string { return filepath.Join(dir, "udp") }

// maxDatagram is the largest UDP payload, with room for the header.
const maxDatagram = 65535 + Header

// Router accepts on a loopback port and joins each connection to the
// container's listener on the port it names, and relays its UDP ports'
// datagrams.
type Router struct {
	l   net.Listener
	dir string
	key []byte
	net Network
	mu  sync.Mutex
	cs  map[net.Conn]struct{}

	ux   *net.UnixConn
	flow func() (*net.UDPConn, error)
	umu  sync.Mutex
	// flows are the UDP ports' flow ports, by container port, kept while
	// the router runs so a client's lookup stays good.
	flows map[int]*net.UDPConn
	// relays are the relays to other stacks' members' UDP ports.
	relays       map[relayKey]*net.UDPConn
	relayClosers []io.Closer
}

// Network locates the router's container among its stack's members: the
// stack's network file and the container's member address. DialFlow opens
// a UDP socket to raddr on a port of another stack's block.
type Network struct {
	File, Address string
	DialFlow      func(stack string, raddr *net.UDPAddr) (net.Conn, error)
}

// Listen starts a router on addr for the port directory dir; flow opens a
// UDP port of the router's stack's block.

func Listen(addr, dir string, flow func() (*net.UDPConn, error), nw Network) (*Router, error) {
	key, err := loadKey(dir)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	os.Remove(UDPRouterPath(dir))
	ux, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: UDPRouterPath(dir), Net: "unixgram"})
	if err != nil {
		l.Close()
		return nil, err
	}
	// A Unix datagram is bounded by the sending socket's buffer.
	ux.SetReadBuffer(4 * maxDatagram)
	ux.SetWriteBuffer(2 * maxDatagram)
	r := &Router{l: l, dir: dir, key: key, net: nw, relays: map[relayKey]*net.UDPConn{}, cs: map[net.Conn]struct{}{}, ux: ux, flow: flow, flows: map[int]*net.UDPConn{}}
	go r.serve()
	go r.fromContainer()
	return r, nil
}

// loadKey reads the relay key in dir, or makes it, so a router restarted
// for a running container keeps the key its processes read.
func loadKey(dir string) ([]byte, error) {
	if b, err := os.ReadFile(KeyPath(dir)); err == nil && len(b) == KeySize {
		return b, nil
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	tmp := KeyPath(dir) + ".new"
	if err := os.WriteFile(tmp, key, 0o600); err != nil {
		return nil, err
	}
	return key, os.Rename(tmp, KeyPath(dir))
}

// foreignTarget reads the rest of a relay request: the member's address
// and port, and whether the request carried the router's relay key.
func (r *Router) foreignTarget(c net.Conn) (netip.Addr, int, error) {
	var b [KeySize + 6]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return netip.Addr{}, 0, err
	}
	if subtle.ConstantTimeCompare(b[:KeySize], r.key) != 1 {
		return netip.Addr{}, 0, errRefused
	}
	return netip.AddrFrom4([4]byte(b[KeySize : KeySize+4])), int(binary.BigEndian.Uint16(b[KeySize+4:])), nil
}

// readPort reads a request's port, two bytes big-endian.
func readPort(c io.Reader) (int, error) {
	var b [2]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(b[:])), nil
}

func (r *Router) Addr() *net.TCPAddr { return r.l.Addr().(*net.TCPAddr) }

// Close stops accepting, ends every joined connection and stops relaying
// datagrams.
func (r *Router) Close() error {
	err := r.l.Close()
	r.mu.Lock()
	for c := range r.cs {
		c.Close()
	}
	r.mu.Unlock()
	r.ux.Close()
	r.umu.Lock()
	for _, c := range r.flows {
		c.Close()
	}
	for _, c := range r.relayClosers {
		c.Close()
	}
	r.umu.Unlock()
	return err
}

// flowPort returns UDP port port's flow port, opening it the first time.
func (r *Router) flowPort(port int) (*net.UDPConn, error) {
	r.umu.Lock()
	defer r.umu.Unlock()
	if c, ok := r.flows[port]; ok {
		return c, nil
	}
	c, err := r.flow()
	if err != nil {
		return nil, err
	}
	r.flows[port] = c
	go r.toContainer(port, c)
	return c, nil
}

// toContainer relays the datagrams the flow port receives to the
// container's socket on port, which drops them when nothing listens there,
// as a closed UDP port does.
func (r *Router) toContainer(port int, c *net.UDPConn) {
	to := &net.UnixAddr{Name: UDPSocketPath(r.dir, port), Net: "unixgram"}
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := c.ReadFromUDPAddrPort(buf[Header:])
		if err != nil {
			return
		}
		a := from.Addr().Unmap()
		if !a.Is4() {
			continue
		}
		v4 := a.As4()
		copy(buf, v4[:])
		binary.BigEndian.PutUint16(buf[4:], from.Port())
		r.ux.WriteToUnix(buf[:Header+n], to)
	}
}

// fromContainer sends the datagrams the container's UDP sockets give it
// from their ports' flow ports.
func (r *Router) fromContainer() {
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := r.ux.ReadFromUnix(buf)
		if err != nil {
			return
		}
		if n < Header || from == nil {
			continue
		}
		port, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(from.Name), "udp-"))
		if err != nil || !strings.HasPrefix(filepath.Base(from.Name), "udp-") {
			continue
		}
		c, err := r.flowPort(port)
		if err != nil {
			continue
		}
		to := netip.AddrPortFrom(netip.AddrFrom4([4]byte(buf[:4])), binary.BigEndian.Uint16(buf[4:]))
		c.WriteToUDPAddrPort(buf[Header:n], to)
	}
}

func (r *Router) track(c net.Conn, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		r.cs[c] = struct{}{}
	} else {
		delete(r.cs, c)
	}
}

func (r *Router) serve() {
	for {
		c, err := r.l.Accept()
		if err != nil {
			return
		}
		go r.join(c)
	}
}

func (r *Router) join(c net.Conn) {
	r.track(c, true)
	defer r.track(c, false)
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var kind [1]byte
	if _, err := io.ReadFull(c, kind[:]); err != nil {
		return
	}
	var up net.Conn
	var err error
	switch kind[0] {
	case Flow, FlowRelay:
		r.lookup(c, kind[0] == FlowRelay)
		return
	case StreamRelay:
		var addr netip.Addr
		var p int
		if addr, p, err = r.foreignTarget(c); err == nil {
			up, err = r.foreign(addr, p)
		}
	case Stream:
		var port int
		if port, err = readPort(c); err == nil {
			if up, err = net.Dial("unix", SocketPath(r.dir, port)); err == nil {
				if _, err = up.Write(clientHeader(c.RemoteAddr())); err != nil {
					up.Close()
				}
			}
		}
	default:
		err = fmt.Errorf("router: request kind %d", kind[0])
	}
	c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Write([]byte{Refused})
		return
	}
	r.track(up, true)
	defer r.track(up, false)
	defer up.Close()
	if _, err := c.Write([]byte{Connected}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
	go func() { io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

// foreign connects to port of the member at addr, which must share a
// network with the router's container: at its host port, or through its
// router.
func (r *Router) foreign(addr netip.Addr, port int) (net.Conn, error) {
	to, err := r.peer(addr)
	if err != nil {
		return nil, err
	}
	if hp, ok := to.Ports[fmt.Sprintf("%d/tcp", port)]; ok {
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
	}
	if to.Router == 0 {
		return nil, errRefused
	}
	return Dial(net.JoinHostPort("127.0.0.1", strconv.Itoa(to.Router)), port)
}

// peer finds the member at addr, which must share a network with the
// router's container or be linked to from it.
func (r *Router) peer(addr netip.Addr) (members.Member, error) {
	if r.net.File == "" {
		return members.Member{}, errRefused
	}
	b, err := os.ReadFile(r.net.File)
	if err != nil {
		return members.Member{}, err
	}
	ms := members.Parse(b)
	self, ok := members.Find(ms, r.net.Address)
	if !ok {
		return members.Member{}, errRefused
	}
	to, ok := members.Find(ms, addr.String())
	if !ok || !members.Sees(self, to) {
		return members.Member{}, errRefused
	}
	return to, nil
}

type relayKey struct {
	addr netip.Addr
	port int
}

// relay returns the port of the router's relay to UDP port port of
// another stack's member at addr, made on the first lookup: a UDP port of
// the router's stack's block, which its container may send to, whose
// datagrams go on to the member's host port or flow port from a socket on
// a port of the member's stack's block, which the member may answer, one
// for each of the container's sockets. A socket's first datagram checks
// again that the member shares a network with the container; the relay
// takes datagrams from the container's own ports alone.
func (r *Router) relay(addr netip.Addr, port int) (int, error) {
	to, err := r.peer(addr)
	if err != nil {
		return 0, err
	}
	if r.net.DialFlow == nil {
		return 0, errRefused
	}
	r.umu.Lock()
	defer r.umu.Unlock()
	key := relayKey{addr, port}
	if pc, ok := r.relays[key]; ok {
		return pc.LocalAddr().(*net.UDPAddr).Port, nil
	}
	var upstream int
	if hp, ok := to.Ports[fmt.Sprintf("%d/udp", port)]; ok {
		upstream = hp
	} else if to.Router != 0 {
		if upstream, err = LookupUDP(net.JoinHostPort("127.0.0.1", strconv.Itoa(to.Router)), port); err != nil {
			return 0, err
		}
	} else {
		return 0, errRefused
	}
	pc, err := r.flow()
	if err != nil {
		return 0, err
	}
	stack := to.Stack
	px := portproxy.ServeUDP(pc, net.JoinHostPort("127.0.0.1", strconv.Itoa(upstream)), func(raddr *net.UDPAddr) (net.Conn, error) {
		if _, err := r.peer(addr); err != nil {
			return nil, err
		}
		// A Service is served outside any sandbox, which answers a
		// datagram from any port; a member's sandbox answers its stack's.
		if stack == members.ServicesStack {
			return net.DialUDP("udp", nil, raddr)
		}
		return r.net.DialFlow(stack, raddr)
	}, r.sends)
	r.relays[key] = pc
	r.relayClosers = append(r.relayClosers, px)
	return pc.LocalAddr().(*net.UDPAddr).Port, nil
}

// sends reports whether a datagram from port of loopback came from the
// router's container: from one of its UDP host ports or flow ports, the
// ports its sockets send from. Another member of the stack, which can
// reach the relay's port, sends from ports of its own.
func (r *Router) sends(from *net.UDPAddr) bool {
	if !from.IP.IsLoopback() {
		return false
	}
	r.umu.Lock()
	for _, c := range r.flows {
		if c.LocalAddr().(*net.UDPAddr).Port == from.Port {
			r.umu.Unlock()
			return true
		}
	}
	r.umu.Unlock()
	if r.net.File == "" {
		return false
	}
	b, err := os.ReadFile(r.net.File)
	if err != nil {
		return false
	}
	self, ok := members.Find(members.Parse(b), r.net.Address)
	if !ok {
		return false
	}
	for p, hp := range self.Ports {
		if strings.HasSuffix(p, "/udp") && hp == from.Port {
			return true
		}
	}
	return false
}

// lookup answers a UDP lookup with the port's flow port.
func (r *Router) lookup(c net.Conn, relay bool) {
	var f int
	if relay {
		addr, p, err := r.foreignTarget(c)
		if err == nil {
			f, err = r.relay(addr, p)
		}
		if err != nil {
			c.Write([]byte{Refused})
			return
		}
	} else {
		port, err := readPort(c)
		if err != nil {
			c.Write([]byte{Refused})
			return
		}
		fc, err := r.flowPort(port)
		if err != nil {
			c.Write([]byte{Refused})
			return
		}
		f = fc.LocalAddr().(*net.UDPAddr).Port
	}
	ans := []byte{Connected, 0, 0}
	binary.BigEndian.PutUint16(ans[1:], uint16(f))
	c.Write(ans)
}

// LookupUDP returns the flow port of UDP port port of the container whose
// router listens on addr, as the container library looks it up.
func LookupUDP(addr string, port int) (int, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	req := binary.BigEndian.AppendUint16([]byte{Flow}, uint16(port))
	if _, err := c.Write(req); err != nil {
		return 0, err
	}
	var ans [3]byte
	if _, err := io.ReadFull(c, ans[:1]); err != nil {
		return 0, err
	}
	if ans[0] != Connected {
		return 0, errRefused
	}
	if _, err := io.ReadFull(c, ans[1:]); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(ans[1:])), nil
}

func closeWrite(c net.Conn) {
	type halfCloser interface{ CloseWrite() error }
	if h, ok := c.(halfCloser); ok {
		h.CloseWrite()
	}
}

// Dial reaches port of the container whose router listens on addr, as the
// container library does.
func Dial(addr string, port int) (net.Conn, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	req := binary.BigEndian.AppendUint16([]byte{Stream}, uint16(port))
	var ans [1]byte
	if _, err := c.Write(req); err == nil {
		_, err = io.ReadFull(c, ans[:])
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	if ans[0] != Connected {
		c.Close()
		return nil, errRefused
	}
	return c, nil
}

var errRefused = &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errors.New("connection refused"))}

// Listener is a listener on a port's socket in a port directory for a
// process outside the container library, which strips the client's
// address the router writes before each connection's stream: its
// connections read the client's stream alone and report the client as
// their remote address.
func Listener(l net.Listener) net.Listener { return clientListener{l} }

type clientListener struct{ net.Listener }

func (l clientListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &clientConn{Conn: c}, nil
}

// clientConn reads the router's header on its first read or the first
// call for its remote address, so a slow client holds up no other
// connection's accept.
type clientConn struct {
	net.Conn
	once   sync.Once
	client net.Addr
	err    error
}

func (c *clientConn) header() error {
	c.once.Do(func() {
		from, err := ReadClient(c.Conn)
		if err != nil {
			c.err = fmt.Errorf("reading the router's client header: %w", err)
			return
		}
		c.client = net.TCPAddrFromAddrPort(from)
	})
	return c.err
}

func (c *clientConn) Read(b []byte) (int, error) {
	if err := c.header(); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *clientConn) RemoteAddr() net.Addr {
	if c.header() != nil {
		return c.Conn.RemoteAddr()
	}
	return c.client
}
