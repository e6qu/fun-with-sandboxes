package egress

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"syscall"
	"time"
)

// SOCKS5 (RFC 1928) message values the proxy reads and writes.
const (
	socksVersion       = 0x05
	socksNoAuth        = 0x00
	socksNoMethod      = 0xff
	socksConnect       = 0x01
	socksIPv4          = 0x01
	socksDomain        = 0x03
	socksIPv6          = 0x04
	socksSucceeded     = 0x00
	socksFailure       = 0x01
	socksNotAllowed    = 0x02
	socksNetUnreach    = 0x03
	socksHostUnreach   = 0x04
	socksRefused       = 0x05
	socksCmdUnsupport  = 0x07
	socksAddrUnsupport = 0x08
)

// greetingTimeout bounds how long a client may take to send its first
// bytes, as the HTTP side bounds its request header.
const greetingTimeout = 30 * time.Second

// sniffer hands HTTP connections to the proxy's HTTP server and serves the
// SOCKS5 ones itself, telling them apart by the first byte: a SOCKS5
// greeting starts with version 5, an HTTP request with a method name.
type sniffer struct {
	net.Listener
	p     *Proxy
	conns chan net.Conn
	done  chan struct{}
}

func newSniffer(l net.Listener, p *Proxy) *sniffer {
	s := &sniffer{Listener: l, p: p, conns: make(chan net.Conn), done: make(chan struct{})}
	go s.accept()
	return s
}

func (s *sniffer) accept() {
	defer close(s.done)
	for {
		c, err := s.Listener.Accept()
		if err != nil {
			return
		}
		go s.sniff(c)
	}
}

func (s *sniffer) sniff(c net.Conn) {
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(greetingTimeout))
	first, err := br.Peek(1)
	if err != nil {
		c.Close()
		return
	}
	c.SetReadDeadline(time.Time{})
	pc := &peekedConn{Conn: c, r: br}
	if first[0] == socksVersion {
		s.p.socks(pc)
		return
	}
	select {
	case s.conns <- pc:
	case <-s.done:
		c.Close()
	}
}

func (s *sniffer) Accept() (net.Conn, error) {
	select {
	case c := <-s.conns:
		return c, nil
	case <-s.done:
		return nil, net.ErrClosed
	}
}

// peekedConn reads first what sniffing buffered.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// socks serves one SOCKS5 client: no authentication, as for the HTTP side,
// and CONNECT alone, through the same dial, so the policy refuses the host
// as it does for HTTP.
func (p *Proxy) socks(c net.Conn) {
	if !p.track(c) {
		c.Close()
		return
	}
	defer p.untrack(c)
	c.SetDeadline(time.Now().Add(greetingTimeout))
	target, err := socksHandshake(c)
	if err != nil {
		return
	}
	up, err := p.dial(context.Background(), "tcp", target)
	if err != nil {
		socksReply(c, socksCode(err), nil)
		return
	}
	if !p.track(up) {
		up.Close()
		return
	}
	defer p.untrack(up)
	if err := socksReply(c, socksSucceeded, up.LocalAddr()); err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
	go func() { io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

// socksHandshake reads the greeting and the request, answering what it must
// refuse, and returns the host:port to connect to.
func socksHandshake(c net.Conn) (string, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return "", err
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	noAuth := false
	for _, m := range methods {
		if m == socksNoAuth {
			noAuth = true
		}
	}
	if !noAuth {
		c.Write([]byte{socksVersion, socksNoMethod})
		return "", errors.New("socks: the client offers no method the proxy takes")
	}
	if _, err := c.Write([]byte{socksVersion, socksNoAuth}); err != nil {
		return "", err
	}
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return "", err
	}
	if req[0] != socksVersion {
		return "", fmt.Errorf("socks: request version %d", req[0])
	}
	var host string
	switch req[3] {
	case socksIPv4, socksIPv6:
		n := net.IPv4len
		if req[3] == socksIPv6 {
			n = net.IPv6len
		}
		ip := make([]byte, n)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case socksDomain:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return "", err
		}
		name := make([]byte, n[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return "", err
		}
		host = string(name)
	default:
		socksReply(c, socksAddrUnsupport, nil)
		return "", fmt.Errorf("socks: address type %d", req[3])
	}
	var port [2]byte
	if _, err := io.ReadFull(c, port[:]); err != nil {
		return "", err
	}
	if req[1] != socksConnect {
		// BIND and UDP ASSOCIATE would open the host to the container.
		socksReply(c, socksCmdUnsupport, nil)
		return "", fmt.Errorf("socks: command %d", req[1])
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

// socksReply answers a request with code and, on success, the address the
// proxy connects from.
func socksReply(c net.Conn, code byte, bound net.Addr) error {
	ip, port := net.IPv4zero.To4(), 0
	if a, ok := bound.(*net.TCPAddr); ok {
		ip, port = a.IP, a.Port
	}
	atyp := byte(socksIPv4)
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else {
		atyp = socksIPv6
	}
	msg := append([]byte{socksVersion, code, 0x00, atyp}, ip...)
	msg = binary.BigEndian.AppendUint16(msg, uint16(port))
	_, err := c.Write(msg)
	return err
}

// socksCode maps a dial failure to its SOCKS5 reply.
func socksCode(err error) byte {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, ErrDenied):
		return socksNotAllowed
	case errors.Is(err, syscall.ECONNREFUSED):
		return socksRefused
	case errors.Is(err, syscall.ENETUNREACH):
		return socksNetUnreach
	case errors.Is(err, syscall.EHOSTUNREACH), errors.As(err, &dnsErr):
		return socksHostUnreach
	}
	return socksFailure
}
