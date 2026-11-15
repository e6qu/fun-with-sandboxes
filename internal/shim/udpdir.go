package shim

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"

	"github.com/e6qu/fun-with-sandboxes/internal/portproxy"
	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// listenUDPDir publishes a UDP port in the port directory dir as the
// container library receives a UDP port without a host port: on the port's
// Unix datagram socket there, the first from lo to hi free, or an
// ephemeral one with lo 0. The container's router delivers each client's
// datagram to that socket after the client's address, and sends on from
// the port's flow port each reply the socket gives it after the address;
// the datagrams between go to upstream from a socket dial opens for each
// client, as portproxy.ServeUDP forwards them.
func listenUDPDir(dir string, lo, hi int, upstream string, dial portproxy.FlowDialer) (io.Closer, int, error) {
	uc, port, err := portproxy.ListenUnixgram(func(p int) string { return router.UDPSocketPath(dir, p) }, lo, hi)
	if err != nil {
		return nil, 0, err
	}
	pc := &dirPacketConn{
		UnixConn: uc, path: router.UDPSocketPath(dir, port),
		router: &net.UnixAddr{Name: router.UDPRouterPath(dir), Net: "unixgram"},
		buf:    make([]byte, router.Header+65535),
	}
	return portproxy.ServeUDP(pc, upstream, dial, nil), port, nil
}

// dirPacketConn is a UDP port's Unix datagram socket in a port directory
// as a packet connection whose peers are the clients the router's headers
// name.
type dirPacketConn struct {
	*net.UnixConn
	path   string
	router *net.UnixAddr
	// buf holds a datagram with its header; one goroutine reads.
	buf []byte
}

var errNotUDPv4 = errors.New("a client the router does not reach: not a UDP IPv4 address")

// ReadFrom reads the next datagram's payload into b and returns the client
// its header names, skipping a datagram too short for one.
func (c *dirPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
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
func (c *dirPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, errNotUDPv4
	}
	ap := ua.AddrPort()
	if a := ap.Addr().Unmap(); a.Is4() {
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
	return 0, errNotUDPv4
}

// Close closes the socket and removes its file.
func (c *dirPacketConn) Close() error {
	err := c.UnixConn.Close()
	os.Remove(c.path)
	return err
}
