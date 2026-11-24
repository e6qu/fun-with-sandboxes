package router

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/e6qu/fun-with-sandboxes/internal/members"
)

// The router answers DNS on UDP at its own port, where the container
// library sends the datagrams a container's processes address to port 53
// of a server outside the machine, which Seatbelt would refuse: a program
// that sends DNS itself, as dig and Go's pure resolver do, reaches the
// router instead. It answers the A and AAAA questions for names the
// container's stack answers, as the library resolves them, and passes any
// other query on to the container's servers: a member of its stack over
// TCP, at its port 53, and a server outside, given internet access; with
// none it can ask, it refuses the query.

// dnsTTL is the time to live of the records the router answers with: the
// stack's members come and go, so a resolver asks again soon.
const dnsTTL = 5

// dnsForwardTimeout bounds one forwarded query to one server.
const dnsForwardTimeout = 5 * time.Second

// DNSStream is the router request that carries DNS over TCP (serveDNSStream).
const DNSStream = 7

// dnsStreamIdle is how long a DNS connection waits for its next query, as
// a server closes an idle one.
const dnsStreamIdle = 30 * time.Second

// dnsInFlight bounds the UDP queries a router answers at once.
const dnsInFlight = 64

// serveDNS answers the queries that arrive on u until it closes.
func (r *Router) serveDNS(u *net.UDPConn) {
	buf := make([]byte, 65535)
	// At most dnsInFlight queries are answered at once, as an answer may
	// wait on an upstream server; past that a query is dropped, which a
	// resolver retries, rather than spawning without bound.
	slots := make(chan struct{}, dnsInFlight)
	for {
		n, from, err := u.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			continue
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			defer func() { <-slots }()
			if a := r.answerDNS(q, false); a != nil {
				u.WriteToUDPAddrPort(a, from)
			}
		}()
	}
}

// serveDNSStream answers the queries a DNS connection carries, each after
// its length in two bytes, until it ends or idles.
func (r *Router) serveDNSStream(c net.Conn) {
	if _, err := c.Write([]byte{Connected}); err != nil {
		return
	}
	for {
		c.SetReadDeadline(time.Now().Add(dnsStreamIdle))
		var l [2]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		a := r.answerDNS(q, true)
		if a == nil {
			return
		}
		if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(a))), a...)); err != nil {
			return
		}
	}
}

// answerDNS is the answer to query q, or nil to send none; one that came
// over TCP goes on over TCP, whose answers are not cut short.
func (r *Router) answerDNS(q []byte, tcp bool) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(q)
	if err != nil || h.Response {
		return nil
	}
	qs, err := p.AllQuestions()
	if err != nil {
		return nil
	}
	if h.OpCode == 0 && len(qs) == 1 && qs[0].Class == dnsmessage.ClassINET {
		if a, ok := r.stackAnswer(h, qs[0]); ok {
			return a
		}
	}
	// The container's servers in turn: one of its stack's members, such as
	// a DNS server it runs, answers over TCP at its port 53, as the router
	// reaches any member's port; any other is asked over the internet.
	asked := false
	for _, s := range r.net.Upstream {
		if c, err := r.foreign(s, 53); err == nil {
			asked = true
			a, err := exchangeConn(c, q, h.ID)
			c.Close()
			if err == nil {
				return a
			}
			continue
		} else if _, err := r.peer(s); err == nil {
			// A member that serves no DNS.
			asked = true
			continue
		}
		if !r.net.Internet {
			continue
		}
		asked = true
		if a := forwardDNS(q, h.ID, []netip.Addr{s}, r.dnsPort(), tcp); a != nil {
			return a
		}
	}
	if asked {
		return dnsReply(h, qs, dnsmessage.RCodeServerFailure, nil)
	}
	return dnsReply(h, qs, dnsmessage.RCodeRefused, nil)
}

// exchangeConn sends q over c, a connection to a DNS server's TCP port,
// and returns its answer.
func exchangeConn(c net.Conn, q []byte, id uint16) ([]byte, error) {
	c.SetDeadline(time.Now().Add(dnsForwardTimeout))
	if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...)); err != nil {
		return nil, err
	}
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	a := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(c, a); err != nil {
		return nil, err
	}
	if len(a) < 2 || binary.BigEndian.Uint16(a) != id {
		return nil, io.ErrUnexpectedEOF
	}
	return a, nil
}

// dnsPort is the upstream servers' port: 53 unless a test serves
// elsewhere.
func (r *Router) dnsPort() int {
	if r.net.DNS != nil && r.net.DNS.port != 0 {
		return r.net.DNS.port
	}
	if r.upstreamPort != 0 {
		return r.upstreamPort
	}
	return 53
}

// stackAnswer answers question q when the container's stack answers its
// name: its member's addresses, as the container sees them, for A and
// AAAA, and no records for any other type. ok is false for a name the
// stack does not answer.
func (r *Router) stackAnswer(h dnsmessage.Header, q dnsmessage.Question) ([]byte, bool) {
	if r.net.File == "" || r.net.Address == "" {
		return nil, false
	}
	b, err := os.ReadFile(r.net.File)
	if err != nil {
		return nil, false
	}
	ms := members.Parse(b)
	self := r.net.Address
	me, ok := members.Find(ms, self)
	if !ok {
		return nil, false
	}
	name := strings.TrimSuffix(q.Name.String(), ".")
	m, ok := members.Lookup(ms, self, members.Unalias(ms, self, name))
	if !ok || m.Alias != "" {
		return nil, false
	}
	var rrs []dnsmessage.Resource
	hdr := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: dnsTTL}
	// A member has addresses alone: any other type has no records.
	//exhaustive:ignore
	switch q.Type {
	case dnsmessage.TypeA:
		if a, err := netip.ParseAddr(m.SeenBy(me)); err == nil && a.Is4() {
			hdr.Type = dnsmessage.TypeA
			rrs = append(rrs, dnsmessage.Resource{Header: hdr, Body: &dnsmessage.AResource{A: a.As4()}})
		}
	case dnsmessage.TypeAAAA:
		if a, err := netip.ParseAddr(m.SeenBy6(me)); err == nil && a.Is6() {
			hdr.Type = dnsmessage.TypeAAAA
			rrs = append(rrs, dnsmessage.Resource{Header: hdr, Body: &dnsmessage.AAAAResource{AAAA: a.As16()}})
		}
	}
	return dnsReply(h, []dnsmessage.Question{q}, dnsmessage.RCodeSuccess, rrs), true
}

// dnsReply is the reply to a query with header h and questions qs.
func dnsReply(h dnsmessage.Header, qs []dnsmessage.Question, rc dnsmessage.RCode, answers []dnsmessage.Resource) []byte {
	m := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID: h.ID, Response: true, OpCode: h.OpCode, Authoritative: rc == dnsmessage.RCodeSuccess,
			RecursionDesired: h.RecursionDesired, RecursionAvailable: true, RCode: rc,
		},
		Questions: qs,
		Answers:   answers,
	}
	b, err := m.Pack()
	if err != nil {
		return nil
	}
	return b
}

// forwardDNS passes query q, whose ID is id, to each server in turn, over
// UDP or TCP, and returns the first answer that carries the ID, as it came.
func forwardDNS(q []byte, id uint16, servers []netip.Addr, port int, tcp bool) []byte {
	for _, s := range servers {
		ctx, cancel := context.WithTimeout(context.Background(), dnsForwardTimeout)
		addr := netip.AddrPortFrom(s, uint16(port)).String()
		var a []byte
		var err error
		if tcp {
			a, err = exchangeRawTCP(ctx, addr, q, id)
		} else {
			a, err = exchangeRaw(ctx, addr, q, id)
		}
		cancel()
		if err == nil {
			return a
		}
	}
	return nil
}

// exchangeRaw sends q to addr over UDP and returns the first datagram
// that answers it.
func exchangeRaw(ctx context.Context, addr string, q []byte, id uint16) ([]byte, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		if n >= 2 && binary.BigEndian.Uint16(buf) == id {
			return append([]byte(nil), buf[:n]...), nil
		}
	}
}

// exchangeRawTCP sends q to addr over TCP and returns its answer.
func exchangeRawTCP(ctx context.Context, addr string, q []byte, id uint16) ([]byte, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...)); err != nil {
		return nil, err
	}
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(c, b); err != nil {
		return nil, err
	}
	if len(b) < 2 || binary.BigEndian.Uint16(b) != id {
		return nil, errAnswer
	}
	return b, nil
}
