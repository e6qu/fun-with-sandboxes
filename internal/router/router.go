// Package router carries traffic to a container's ports that have no host
// port. The container library listens on such a port with a Unix socket in
// the container's port directory, which no other container can reach.
//
// For TCP, a client sends the port it wants to the container's router, a
// loopback port every member of its stack may reach, and the router joins
// the connection to the socket.
//
// For UDP, the router gives each such port a flow port, a loopback UDP port
// of the stack's block, and relays datagrams between it and the port's Unix
// datagram socket. A client asks the router for the flow port once, then
// exchanges datagrams with it as with any host port.
package router

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A request is the container port, two bytes big-endian; the answer is one
// byte, Connected or Refused, after which the connection carries the
// stream. A request for port 0, which no TCP port is, is a UDP lookup: two
// more bytes name the UDP port, and Connected is followed by its flow port,
// two bytes big-endian.
const (
	Connected = 0
	Refused   = 1
)

// Header is the size of what the router puts before a datagram on the Unix
// side: the IPv4 address and port the datagram came from, to the
// container, or goes to, from it.
const Header = 6

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
	mu  sync.Mutex
	cs  map[net.Conn]struct{}

	ux   *net.UnixConn
	flow func() (*net.UDPConn, error)
	umu  sync.Mutex
	// flows are the UDP ports' flow ports, by container port, kept while
	// the router runs so a client's lookup stays good.
	flows map[int]*net.UDPConn
}

// Listen starts a router on addr for the port directory dir; flow opens a
// UDP port's flow port.
func Listen(addr, dir string, flow func() (*net.UDPConn, error)) (*Router, error) {
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
	r := &Router{l: l, dir: dir, cs: map[net.Conn]struct{}{}, ux: ux, flow: flow, flows: map[int]*net.UDPConn{}}
	go r.serve()
	go r.fromContainer()
	return r, nil
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
	var req [2]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(req[:]))
	if port == 0 {
		r.lookup(c)
		return
	}
	c.SetReadDeadline(time.Time{})
	up, err := net.Dial("unix", SocketPath(r.dir, port))
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

// lookup answers a UDP lookup with the port's flow port.
func (r *Router) lookup(c net.Conn) {
	var req [2]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(req[:]))
	if port == 0 {
		c.Write([]byte{Refused})
		return
	}
	f, err := r.flowPort(port)
	if err != nil {
		c.Write([]byte{Refused})
		return
	}
	ans := []byte{Connected, 0, 0}
	binary.BigEndian.PutUint16(ans[1:], uint16(f.LocalAddr().(*net.UDPAddr).Port))
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
	req := []byte{0, 0, 0, 0}
	binary.BigEndian.PutUint16(req[2:], uint16(port))
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
	var req [2]byte
	binary.BigEndian.PutUint16(req[:], uint16(port))
	var ans [1]byte
	if _, err := c.Write(req[:]); err == nil {
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
