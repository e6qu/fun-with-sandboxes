// Package netstat counts a container's network traffic from the kernel's
// per-socket statistics, the counters nettop reads: macOS keeps rx and tx
// packets and bytes for every TCP and UDP socket and reports them, with the
// socket's addresses and the pid that last used it, on the network
// statistics control socket, which needs no privilege on macOS.
//
// A container's traffic is that of its processes' sockets, and of its
// router's, less the connections between two of its own sockets, which in
// Docker cross the container's loopback and not its eth0.
package netstat

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"sync"

	"github.com/e6qu/fun-with-sandboxes/internal/procs"
)

// Counts are the packets and bytes a container received and sent.
type Counts struct {
	RxPackets, RxBytes, TxPackets, TxBytes uint64
}

func (c *Counts) add(o Counts) {
	c.RxPackets += o.RxPackets
	c.RxBytes += o.RxBytes
	c.TxPackets += o.TxPackets
	c.TxBytes += o.TxBytes
}

// The network statistics protocol, from XNU's bsd/net/ntstat.h.
const (
	msgSuccess    = 0
	msgError      = 1
	msgAddAllSrcs = 1002
	msgRemSrc     = 1003
	msgGetUpdate  = 1007
	msgSrcAdded   = 10001
	msgSrcRemoved = 10002
	msgSrcUpdate  = 10006

	providerTCP = 2
	providerUDP = 4

	flagContinuation = 1 << 1
	flagClosing      = 1 << 2

	srcRefAll = ^uint64(0)

	hdrLen = 16
	// An update carries the header, the source's reference, its event
	// flags, its counts (nstat_counts, 112 bytes), its provider and four
	// reserved bytes before the provider's descriptor.
	updateCounts = 32
	updateProv   = 144
	updateData   = 152
)

// Where nstat_tcp_descriptor and nstat_udp_descriptor hold the fields read,
// the same from macOS 15 on.
var layouts = map[uint32]struct{ pid, local, remote int }{
	providerTCP: {pid: 116, local: 124, remote: 152},
	providerUDP: {pid: 128, local: 56, remote: 84},
}

// sockaddrLen is a sockaddr_in6's size, the room each address takes.
const sockaddrLen = 28

// A source is one socket as the kernel reports it.
type source struct {
	ref           uint64
	pid           int
	local, remote netip.AddrPort
	counts        Counts
}

type message struct {
	typ     uint32
	context uint64
	flags   uint16
	ref     uint64
	errno   uint32
	src     *source
}

// parse splits a read from the control socket into its messages, as the
// kernel puts several in one.
func parse(b []byte) ([]message, error) {
	var out []message
	for len(b) > 0 {
		if len(b) < hdrLen {
			return out, fmt.Errorf("network statistics: a truncated message header")
		}
		n := int(binary.LittleEndian.Uint16(b[12:]))
		if n < hdrLen || n > len(b) {
			return out, fmt.Errorf("network statistics: a message of %d bytes in %d", n, len(b))
		}
		m, err := parseOne(b[:n])
		if err != nil {
			return out, err
		}
		out = append(out, m)
		b = b[n:]
	}
	return out, nil
}

func parseOne(b []byte) (message, error) {
	le := binary.LittleEndian
	m := message{context: le.Uint64(b), typ: le.Uint32(b[8:]), flags: le.Uint16(b[14:])}
	switch m.typ {
	case msgError:
		if len(b) >= hdrLen+4 {
			m.errno = le.Uint32(b[hdrLen:])
		}
	case msgSrcAdded, msgSrcRemoved:
		if len(b) < hdrLen+8 {
			return m, fmt.Errorf("network statistics: a removal of %d bytes", len(b))
		}
		m.ref = le.Uint64(b[hdrLen:])
	case msgSrcUpdate:
		if len(b) < updateData {
			return m, fmt.Errorf("network statistics: an update of %d bytes", len(b))
		}
		m.ref = le.Uint64(b[hdrLen:])
		provider := le.Uint32(b[updateProv:])
		l, ok := layouts[provider]
		if !ok {
			return m, nil
		}
		d := b[updateData:]
		if len(d) < l.remote+sockaddrLen || len(d) < l.pid+4 {
			return m, fmt.Errorf("network statistics: provider %d's descriptor of %d bytes is shorter than this kernel's layout", provider, len(d))
		}
		c := b[updateCounts:]
		m.src = &source{
			ref: m.ref, pid: int(le.Uint32(d[l.pid:])),
			local: sockaddr(d[l.local:]), remote: sockaddr(d[l.remote:]),
			counts: Counts{RxPackets: le.Uint64(c), RxBytes: le.Uint64(c[8:]), TxPackets: le.Uint64(c[16:]), TxBytes: le.Uint64(c[24:])},
		}
	}
	return m, nil
}

// sockaddr reads a sockaddr_in or sockaddr_in6, an IPv4-mapped address as
// its IPv4 one.
func sockaddr(b []byte) netip.AddrPort {
	port := binary.BigEndian.Uint16(b[2:])
	switch b[1] {
	case 2:
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), port)
	case 30:
		return netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[8:24])).Unmap(), port)
	}
	return netip.AddrPort{}
}

// tracked is one of the container's sockets.
type tracked struct {
	source
	// internal marks a connection between two of the container's own
	// sockets, whose peer closed first.
	internal bool
}

type endpoint struct {
	pid  int
	port uint16
}

type pair struct{ local, remote netip.AddrPort }

// Account sums the traffic of a container's sockets from the kernel's
// reports.
type Account struct {
	// tag is the environment entry every container process starts with.
	tag string

	mu      sync.Mutex
	members map[int]bool
	// tagged caches whether processes not among the members carry the tag,
	// until the members next change.
	tagged map[int]bool
	// endpoints are sockets of processes outside the container that carry
	// its traffic, by local port: its router's.
	endpoints map[endpoint]bool
	live      map[uint64]*tracked
	byPair    map[pair]*tracked
	// closing are the container's sockets that closed with no peer found,
	// by their final addresses: the kernel announces a client's socket
	// before it connects, so its peer's addresses may be the ones it had
	// then until the next poll. total settles them once a poll has brought
	// every socket's addresses up to date.
	closing map[pair]*tracked
	// gone is the traffic of the container's sockets that closed.
	gone Counts
}

// NewAccount counts the sockets of processes whose environment holds tag,
// and of the members and endpoints it is given.
func NewAccount(tag string) *Account {
	return &Account{
		tag: tag, members: map[int]bool{}, tagged: map[int]bool{}, endpoints: map[endpoint]bool{},
		live: map[uint64]*tracked{}, byPair: map[pair]*tracked{}, closing: map[pair]*tracked{},
	}
}

// SetMembers sets the container's processes.
func (a *Account) SetMembers(pids []int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.members = make(map[int]bool, len(pids))
	for _, p := range pids {
		a.members[p] = true
	}
	clear(a.tagged)
}

// AddEndpoint counts the sockets pid holds on local port as the
// container's.
func (a *Account) AddEndpoint(pid int, port uint16) {
	a.mu.Lock()
	a.endpoints[endpoint{pid, port}] = true
	a.mu.Unlock()
}

func (a *Account) ownsLocked(s *source) bool {
	if a.members[s.pid] || a.endpoints[endpoint{s.pid, s.local.Port()}] {
		return true
	}
	if a.tag == "" || s.pid <= 0 {
		return false
	}
	t, ok := a.tagged[s.pid]
	if !ok {
		env, _ := procs.Environ(s.pid)
		for _, kv := range env {
			if kv == a.tag {
				t = true
				break
			}
		}
		a.tagged[s.pid] = t
	}
	return t
}

func (a *Account) peerLocked(t *tracked) *tracked {
	if !t.remote.IsValid() || t.remote.Port() == 0 {
		return nil
	}
	return a.byPair[pair{t.remote, t.local}]
}

// update records a socket's latest report, a closing one's counts final,
// and says whether the socket is the container's: one is once one of its
// processes used it. It forgets any other socket, which the caller stops
// watching.
func (a *Account) update(s source, closing bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.live[s.ref]
	if t == nil {
		if !a.ownsLocked(&s) {
			return false
		}
		t = &tracked{}
		a.live[s.ref] = t
	} else if a.byPair[pair{t.local, t.remote}] == t {
		delete(a.byPair, pair{t.local, t.remote})
	}
	t.source = s
	a.byPair[pair{s.local, s.remote}] = t
	if closing {
		a.closeLocked(t)
	}
	return true
}

func (a *Account) remove(ref uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.live[ref]; t != nil {
		a.closeLocked(t)
	}
}

// maxClosing bounds the sockets awaiting a poll, which nothing makes while
// no one asks for the container's stats.
const maxClosing = 4096

func (a *Account) closeLocked(t *tracked) {
	delete(a.live, t.ref)
	if a.byPair[pair{t.local, t.remote}] == t {
		delete(a.byPair, pair{t.local, t.remote})
	}
	if p := a.peerLocked(t); p != nil {
		p.internal = true
		return
	}
	if p := a.closing[pair{t.remote, t.local}]; p != nil {
		delete(a.closing, pair{t.remote, t.local})
		return
	}
	if t.internal {
		return
	}
	if !t.remote.IsValid() || t.remote.Port() == 0 {
		// A socket with no peer, as an unconnected UDP one has.
		a.gone.add(t.counts)
		return
	}
	if len(a.closing) >= maxClosing {
		a.settleLocked()
	}
	a.closing[pair{t.local, t.remote}] = t
}

// settleLocked counts the closed sockets whose peer is none of the
// container's live ones, and leaves out the rest, whose peer it marks.
func (a *Account) settleLocked() {
	for k, t := range a.closing {
		if p := a.peerLocked(t); p != nil {
			p.internal = true
		} else {
			a.gone.add(t.counts)
		}
		delete(a.closing, k)
	}
}

// total is the container's traffic as of the latest reports.
// total is the container's traffic as of the latest reports, which a poll
// of every socket must precede.
func (a *Account) total() Counts {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settleLocked()
	c := a.gone
	for _, t := range a.live {
		if !t.internal && a.peerLocked(t) == nil {
			c.add(t.counts)
		}
	}
	return c
}
