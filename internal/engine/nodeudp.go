package engine

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"strconv"

	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// listenUDPOnNode listens on UDP port p of the engine's node, as
// listenOnNode does on a TCP one: for an engine nested in a privileged
// container, that container's UDP port p, at its host port or on the
// port's datagram socket in the container's port directory, where its
// router delivers the datagrams of the members of its networks; else port
// p of the host's loopback.
func (e *Engine) listenUDPOnNode(p int) (net.PacketConn, error) {
	n := e.cfg.Nested
	if n == nil {
		return net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	}
	hp, dir, err := n.Port("udp", p)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
	}
	path := router.UDPSocketPath(dir, p)
	os.Remove(path)
	uc, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	return &routedPacketConn{UnixConn: uc, path: path, router: &net.UnixAddr{Name: router.UDPRouterPath(dir), Net: "unixgram"},
		buf: make([]byte, router.Header+65535)}, nil
}

// routedPacketConn is a UDP port's datagram socket in a port directory as
// a packet connection whose peers are the clients the router names: each
// datagram, either way, carries the client's IPv4 address and port before
// its payload, as router.Header lays them out.
type routedPacketConn struct {
	*net.UnixConn
	path   string
	router *net.UnixAddr
	// buf holds a datagram with its header; one goroutine reads.
	buf []byte
}

var errNotRouted = errors.New("the router reaches IPv4 UDP clients alone")

// ReadFrom reads the next datagram's payload into b and returns the client
// its header names, skipping one too short for a header.
func (c *routedPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, _, err := c.UnixConn.ReadFromUnix(c.buf)
		if err != nil {
			return 0, nil, err
		}
		if n < router.Header {
			continue
		}
		client := netip.AddrPortFrom(netip.AddrFrom4([4]byte(c.buf[:4])), binary.BigEndian.Uint16(c.buf[4:router.Header]))
		return copy(b, c.buf[router.Header:n]), net.UDPAddrFromAddrPort(client), nil
	}
}

// WriteTo sends b to the router for the client at addr.
func (c *routedPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, errNotRouted
	}
	ap := ua.AddrPort()
	a := ap.Addr().Unmap()
	if !a.Is4() {
		return 0, errNotRouted
	}
	v4 := a.As4()
	msg := make([]byte, router.Header+len(b))
	copy(msg, v4[:])
	binary.BigEndian.PutUint16(msg[4:router.Header], ap.Port())
	copy(msg[router.Header:], b)
	if _, err := c.UnixConn.WriteToUnix(msg, c.router); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Close closes the socket and removes its file.
func (c *routedPacketConn) Close() error {
	err := c.UnixConn.Close()
	os.Remove(c.path)
	return err
}
