package router

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Resolve asks the router to resolve a name through the container's DNS
// servers, for a container with internet access whose engine gave it
// servers, search domains or options of its own (Network.DNS): one byte
// for the family, 0 for any, 4 or 6, one for the name's length, then the
// name. Connected is followed by the number of addresses, one byte, and
// each address, a byte of its length, 4 or 16, then its bytes. A name that
// resolves to no address is Refused.
const Resolve = 6

// ResolveTimeout bounds a Resolve request, whatever the container's
// servers, search domains and options make of it.
const ResolveTimeout = 30 * time.Second

// maxAddresses is the most addresses a Resolve answer carries.
const maxAddresses = 32

// DNS is how a container resolves names that neither its /etc/hosts nor
// its stack's members answer: the servers, search domains and options of
// its /etc/resolv.conf, as a stub resolver reads them. It covers ndots,
// timeout and attempts of the options; it cannot honour the others
// (rotate, edns0, single-request and the like), which change only how
// the queries go out.
type DNS struct {
	Servers  []netip.Addr
	Search   []string `json:",omitempty"`
	Ndots    int
	Timeout  time.Duration
	Attempts int
	// port is the servers' port, 53 unless a test serves elsewhere.
	port int
	// member connects to the TCP port 53 of a server that is a member of
	// the container's stack, which ok reports; the router reaches such a
	// server whatever the container's internet access (Router.resolve).
	member func(server netip.Addr) (c net.Conn, ok bool, err error)
	// offline marks a container without internet access, whose servers
	// outside the machine are out of reach.
	offline bool
}

// ParseResolvConf reads the servers, search domains and options of a
// resolv.conf, with a stub resolver's defaults for what it leaves out: an
// ndots of 1, a timeout of 5 seconds and 2 attempts. The last search or
// domain line names the search domains.
func ParseResolvConf(b []byte) DNS {
	d := DNS{Ndots: 1, Timeout: 5 * time.Second, Attempts: 2}
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "nameserver":
			if a, err := netip.ParseAddr(f[1]); err == nil {
				d.Servers = append(d.Servers, a)
			}
		case "domain", "search":
			d.Search = nil
			for _, s := range f[1:] {
				if s = strings.TrimSuffix(s, "."); s != "" {
					d.Search = append(d.Search, s)
				}
			}
		case "options":
			for _, o := range f[1:] {
				k, v, ok := strings.Cut(o, ":")
				n, err := strconv.Atoi(v)
				if !ok || err != nil || n < 0 {
					continue
				}
				switch k {
				case "ndots":
					d.Ndots = min(n, 15)
				case "timeout":
					d.Timeout = time.Duration(min(max(n, 1), 30)) * time.Second
				case "attempts":
					d.Attempts = min(max(n, 1), 5)
				}
			}
		}
	}
	return d
}

// candidates are the absolute names a stub resolver tries for name, in
// order: name as it is first when it has at least ndots dots, then name in
// each search domain, then name as it is when it had fewer. A name that
// ends in a dot is absolute already.
func (d *DNS) candidates(name string) []string {
	if strings.HasSuffix(name, ".") {
		return []string{name}
	}
	asIs := strings.Count(name, ".") >= d.Ndots
	var out []string
	if asIs {
		out = append(out, name+".")
	}
	for _, s := range d.Search {
		out = append(out, name+"."+s+".")
	}
	if !asIs {
		out = append(out, name+".")
	}
	return out
}

// Lookup resolves name through the servers, trying each candidate name in
// turn until one has addresses of family (0 for any, 4 or 6), IPv4 ones
// first. A server that fails or does not answer passes the query to the
// next; one that answers, with addresses or that the name has none,
// settles the candidate.
func (d *DNS) Lookup(ctx context.Context, family byte, name string) []netip.Addr {
	var types []dnsmessage.Type
	switch family {
	case 0:
		types = []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA}
	case 4:
		types = []dnsmessage.Type{dnsmessage.TypeA}
	case 6:
		types = []dnsmessage.Type{dnsmessage.TypeAAAA}
	default:
		return nil
	}
	for _, fqdn := range d.candidates(name) {
		// A search domain can make a name longer than DNS takes; the
		// other candidates still stand.
		n, err := dnsmessage.NewName(fqdn)
		if err != nil {
			continue
		}
		var out []netip.Addr
		for _, t := range types {
			out = append(out, d.query(ctx, n, t)...)
		}
		if len(out) > 0 {
			return out[:min(len(out), maxAddresses)]
		}
		if ctx.Err() != nil {
			return nil
		}
	}
	return nil
}

// query asks the servers in turn, each up to its attempts, for name's
// records of type t, until one answers.
func (d *DNS) query(ctx context.Context, name dnsmessage.Name, t dnsmessage.Type) []netip.Addr {
	for range max(d.Attempts, 1) {
		for _, s := range d.Servers {
			addrs, err := d.exchange(ctx, s, name, t)
			if err == nil {
				return addrs
			}
			if ctx.Err() != nil {
				return nil
			}
		}
	}
	return nil
}

var errAnswer = errors.New("router: no usable DNS answer")

// memberConn connects to server's TCP port 53 when it is a member of the
// container's stack, which ok reports.
func (d *DNS) memberConn(server netip.Addr) (net.Conn, bool, error) {
	if d.member == nil {
		return nil, false, nil
	}
	return d.member(server)
}

// exchange sends one query to server, over UDP, and over TCP when the UDP
// answer comes truncated. A server that answers that the name does not
// exist, or has no such records, answers with no addresses.
func (d *DNS) exchange(ctx context.Context, server netip.Addr, name dnsmessage.Name, t dnsmessage.Type) ([]netip.Addr, error) {
	var idb [2]byte
	rand.Read(idb[:])
	id := binary.BigEndian.Uint16(idb[:])
	q, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: t, Class: dnsmessage.ClassINET}},
	}).Pack()
	if err != nil {
		return nil, err
	}
	port := d.port
	if port == 0 {
		port = 53
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var m dnsmessage.Message
	if c, ok, err := d.memberConn(server); ok {
		if err != nil {
			return nil, err
		}
		answer, err := exchangeConn(c, q, id)
		c.Close()
		if err != nil {
			return nil, err
		}
		if err := m.Unpack(answer); err != nil {
			return nil, err
		}
	} else {
		if d.offline {
			return nil, errAnswer
		}
		addr := netip.AddrPortFrom(server, uint16(port)).String()
		var dialer net.Dialer
		answer, err := d.udp(ctx, &dialer, addr, q, id)
		if err != nil {
			return nil, err
		}
		if err := m.Unpack(answer); err != nil {
			return nil, err
		}
		if m.Truncated {
			if answer, err = d.tcp(ctx, &dialer, addr, q); err != nil {
				return nil, err
			}
			if err := m.Unpack(answer); err != nil {
				return nil, err
			}
		}
	}
	if m.ID != id || !m.Response || len(m.Questions) != 1 || m.Questions[0].Type != t || !strings.EqualFold(m.Questions[0].Name.String(), name.String()) {
		return nil, errAnswer
	}
	// An answer or a name that does not exist; any other code is a
	// server's failure.
	//exhaustive:ignore
	switch m.RCode {
	case dnsmessage.RCodeSuccess, dnsmessage.RCodeNameError:
	default:
		return nil, errAnswer
	}
	var out []netip.Addr
	for _, rr := range m.Answers {
		switch b := rr.Body.(type) {
		case *dnsmessage.AResource:
			if t == dnsmessage.TypeA {
				out = append(out, netip.AddrFrom4(b.A))
			}
		case *dnsmessage.AAAAResource:
			if t == dnsmessage.TypeAAAA {
				out = append(out, netip.AddrFrom16(b.AAAA))
			}
		}
	}
	return out, nil
}

// udp sends query q and returns the first answer that carries its ID.
func (d *DNS) udp(ctx context.Context, dialer *net.Dialer, addr string, q []byte, id uint16) ([]byte, error) {
	c, err := dialer.DialContext(ctx, "udp", addr)
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
			return buf[:n], nil
		}
	}
}

// tcp sends query q over TCP, each message after its length.
func (d *DNS) tcp(ctx context.Context, dialer *net.Dialer, addr string, q []byte) ([]byte, error) {
	c, err := dialer.DialContext(ctx, "tcp", addr)
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
	_, err = io.ReadFull(c, b)
	return b, err
}

// resolve answers a Resolve request.
func (r *Router) resolve(c net.Conn) {
	var h [2]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return
	}
	name := make([]byte, h[1])
	if _, err := io.ReadFull(c, name); err != nil {
		return
	}
	var addrs []netip.Addr
	if r.net.DNS != nil && len(name) > 0 {
		// The container's servers that are its stack's members it asks
		// over TCP through the router, with or without internet access;
		// the others only with it.
		d := *r.net.DNS
		d.offline = !r.net.Internet
		d.member = func(s netip.Addr) (net.Conn, bool, error) {
			if _, err := r.peer(s); err != nil {
				return nil, false, nil
			}
			c, err := r.foreign(s, 53)
			return c, true, err
		}
		askable := r.net.Internet
		for _, s := range d.Servers {
			if _, err := r.peer(s); err == nil {
				askable = true
			}
		}
		if askable {
			ctx, cancel := context.WithTimeout(context.Background(), ResolveTimeout)
			addrs = d.Lookup(ctx, h[0], string(name))
			cancel()
		}
	}
	c.Write(resolveAnswer(addrs))
}

// resolveAnswer is the answer to a Resolve request that found addrs.
func resolveAnswer(addrs []netip.Addr) []byte {
	if len(addrs) == 0 {
		return []byte{Refused}
	}
	out := []byte{Connected, byte(len(addrs))}
	for _, a := range addrs {
		b := a.Unmap().AsSlice()
		out = append(append(out, byte(len(b))), b...)
	}
	return out
}

// LookupName resolves name through the router that listens on addr, as
// the container library asks it: family is 0 for any, 4 or 6.
func LookupName(addr string, family byte, name string) ([]netip.Addr, error) {
	if len(name) == 0 || len(name) > 255 {
		return nil, errRefused
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err := c.Write(append([]byte{Resolve, family, byte(len(name))}, name...)); err != nil {
		return nil, err
	}
	var h [2]byte
	if _, err := io.ReadFull(c, h[:1]); err != nil {
		return nil, err
	}
	if h[0] != Connected {
		return nil, errRefused
	}
	if _, err := io.ReadFull(c, h[1:]); err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, h[1])
	for range int(h[1]) {
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return nil, err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(c, b); err != nil {
			return nil, err
		}
		a, ok := netip.AddrFromSlice(b)
		if !ok {
			return nil, errAnswer
		}
		out = append(out, a)
	}
	return out, nil
}
