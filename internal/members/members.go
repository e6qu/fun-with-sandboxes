// Package members reads and writes a stack's network file: a line per
// member of the stack, of its member address, the names it answers to, the
// host ports behind its ports, its router's port, the networks it is on,
// whether it belongs to another stack, that stack, the aliases links give
// it, its addresses on networks with subnets of their own, and, for a pod
// of another node, its stack's block of host ports, tab-separated. The engine writes it;
// the container library, the container's router and the stack's egress
// proxy read it.
package members

import (
	"bytes"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ServicesStack is the stack of the members that are a network's Services,
// which the engine serves on loopback, outside any sandbox: a datagram may
// reach one from any port.
const ServicesStack = "services"

type Member struct {
	Address string
	Names   []string
	// Ports maps a container port, as "5432/tcp", to its host port.
	Ports map[string]int
	// Router is the port of the member's router (package router), which
	// reaches its TCP ports that have no host port; 0 for none.
	Router int
	// Networks are the IDs of the networks the member is on; members
	// reach one another by name and address on a network they share.
	Networks []string
	// Foreign marks a member of another stack, on a network it shares
	// with this one's members. Its ports lie in its own stack's block,
	// which a container reaches only through its router.
	Foreign bool
	// Stack names a foreign member's stack, whose block a datagram to it
	// is sent from, so that its replies may return; ServicesStack for a
	// Service, which the engine serves outside any sandbox.
	Stack string
	// Links are the names the member answers to for one member alone, as
	// a container's links name others; a legacy link also lets the member
	// that makes it reach this one where they share no network.
	Links []Link
	// Subnets are the member's addresses on the networks it is on that
	// have subnets of their own, IPv4 and IPv6, each with its subnet's
	// length; on every other network the member is at Address.
	Subnets []NetAddress
	// Only6 are the networks the member is on that are IPv6-only: it has
	// no IPv4 address there, not even its member address. A network with
	// an IPv6 subnet and no IPv4 one of its own is not one of them: its
	// members reach one another over IPv4 at their member addresses.
	Only6 []string
	// Search is the search domains the member's resolver completes a name
	// with, and Ndots how many dots a name needs to be tried as it is
	// first, as its resolv.conf names them: the stack's egress proxy
	// completes the names a client asks it for as the client would.
	Search []string
	Ndots  int
	// Block is the host ports of a foreign member's stack where no
	// allocator of the reader's engine knows that stack, as for a pod of
	// another node: a datagram to the member is sent from a free port of
	// it, so that its replies may return.
	Block Block
	// Alias marks names that stand for another name, as a DNS CNAME
	// record's do, as an ExternalName Service's: the member has no
	// address or ports, and its names resolve as Alias does. Its line's
	// address field is "=" and Alias, which the container library, reading
	// IPv4 addresses there alone, passes over.
	Alias string
}

// Block is a range of host ports, Limit exclusive; the zero Block is none.
type Block struct {
	Base, Limit int
}

func (b Block) String() string { return fmt.Sprintf("%d-%d", b.Base, b.Limit) }

// Contains reports whether port lies in b.
func (b Block) Contains(port int) bool { return port >= b.Base && port < b.Limit }

// IsZero reports whether b is no block.
func (b Block) IsZero() bool { return b.Limit <= b.Base }

func parseBlock(s string) Block {
	lo, hi, ok := strings.Cut(s, "-")
	b, errB := strconv.Atoi(lo)
	l, errL := strconv.Atoi(hi)
	if !ok || errB != nil || errL != nil || b <= 0 || l <= b || l > 65536 {
		return Block{}
	}
	return Block{Base: b, Limit: l}
}

// NetAddress is a member's address on a network with a subnet.
type NetAddress struct {
	Network string
	Address netip.Prefix
}

// Has reports whether a is one of the member's addresses.
func (m Member) Has(a string) bool {
	if m.Alias != "" {
		return false
	}
	if m.Address == a {
		return true
	}
	ip, err := netip.ParseAddr(a)
	if err != nil {
		return false
	}
	for _, s := range m.Subnets {
		if s.Address.Addr() == ip {
			return true
		}
	}
	return false
}

// On is the member's IPv4 address on network: its own there on a network
// with an IPv4 subnet, none on an IPv6-only network, where it holds an
// IPv6 address alone, else its member address.
func (m Member) On(network string) string {
	if slices.Contains(m.Only6, network) {
		return ""
	}
	for _, s := range m.Subnets {
		if s.Network == network && s.Address.Addr().Is4() {
			return s.Address.Addr().String()
		}
	}
	return m.Address
}

// On6 is the member's IPv6 address on network, or "" where it holds none.
func (m Member) On6(network string) string {
	for _, s := range m.Subnets {
		if s.Network == network && s.Address.Addr().Is6() {
			return s.Address.Addr().String()
		}
	}
	return ""
}

// SeenBy is the member's IPv4 address as viewer reaches it, as the
// container library resolves its name: on the first of its networks the
// viewer is on too that gives it one; none where every such network is
// IPv6-only; else, where they share none, its member address.
func (m Member) SeenBy(viewer Member) string {
	shared := false
	for _, n := range m.Networks {
		if slices.Contains(viewer.Networks, n) {
			shared = true
			if a := m.On(n); a != "" {
				return a
			}
		}
	}
	if shared {
		return ""
	}
	return m.Address
}

// SeenBy6 is the member's IPv6 address as viewer reaches it: on the first
// of its networks the viewer is on too that gives it one, or "".
func (m Member) SeenBy6(viewer Member) string {
	for _, n := range m.Networks {
		if slices.Contains(viewer.Networks, n) {
			if a := m.On6(n); a != "" {
				return a
			}
		}
	}
	return ""
}

// InSubnet reports whether ip lies in the subnet of a network the members
// are on.
func InSubnet(ms []Member, ip netip.Addr) bool {
	for _, m := range ms {
		for _, s := range m.Subnets {
			if s.Address.Masked().Contains(ip) {
				return true
			}
		}
	}
	return false
}

// only6Mark stands where an address would, in a member's subnets, for a
// network that is IPv6-only.
const only6Mark = "-"

// Link is an alias the member at Viewer knows another member by.
type Link struct {
	Viewer, Alias string
}

func Format(ms []Member) []byte {
	var b bytes.Buffer
	for _, m := range ms {
		keys := make([]string, 0, len(m.Ports))
		for k := range m.Ports {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ports := make([]string, len(keys))
		for i, k := range keys {
			ports[i] = fmt.Sprintf("%s=%d", k, m.Ports[k])
		}
		foreign := 0
		if m.Foreign {
			foreign = 1
		}
		links := make([]string, len(m.Links))
		for i, l := range m.Links {
			links[i] = l.Viewer + "/" + l.Alias
		}
		address := m.Address
		if m.Alias != "" {
			address = "=" + m.Alias
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%s\t%d\t%s\t%s", address, strings.Join(m.Names, " "), strings.Join(ports, ","), m.Router, strings.Join(m.Networks, " "), foreign, m.Stack, strings.Join(links, " "))
		if len(m.Subnets) > 0 || len(m.Only6) > 0 || !m.Block.IsZero() || len(m.Search) > 0 {
			subnets := make([]string, len(m.Subnets), len(m.Subnets)+len(m.Only6))
			for i, s := range m.Subnets {
				subnets[i] = s.Network + "=" + s.Address.String()
			}
			// only6Mark, which no reader of addresses takes for one.
			for _, n := range m.Only6 {
				subnets = append(subnets, n+"="+only6Mark)
			}
			fmt.Fprintf(&b, "\t%s", strings.Join(subnets, " "))
		}
		if !m.Block.IsZero() || len(m.Search) > 0 {
			b.WriteByte('\t')
			if !m.Block.IsZero() {
				b.WriteString(m.Block.String())
			}
		}
		if len(m.Search) > 0 {
			// An eleventh field, which the container library, reading its
			// own resolv.conf, passes over.
			fmt.Fprintf(&b, "\tndots:%d %s", m.Ndots, strings.Join(m.Search, " "))
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// Parse reads a network file, leaving out lines it cannot read.
func Parse(b []byte) []Member {
	var out []Member
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 6 || len(f) > 11 || f[0] == "" || f[0] == "=" {
			continue
		}
		m := Member{Address: f[0], Names: strings.Fields(f[1]), Ports: map[string]int{}, Networks: strings.Fields(f[4]), Foreign: f[5] == "1"}
		if alias, ok := strings.CutPrefix(f[0], "="); ok {
			m.Address, m.Alias = "", alias
		}
		if len(f) >= 7 {
			m.Stack = f[6]
		}
		if len(f) >= 10 && f[9] != "" {
			m.Block = parseBlock(f[9])
		}
		if len(f) == 11 {
			for _, t := range strings.Fields(f[10]) {
				if v, ok := strings.CutPrefix(t, "ndots:"); ok {
					m.Ndots, _ = strconv.Atoi(v)
				} else {
					m.Search = append(m.Search, t)
				}
			}
		}
		if len(f) >= 9 {
			for _, t := range strings.Fields(f[8]) {
				n, a, ok := strings.Cut(t, "=")
				if !ok || n == "" {
					continue
				}
				if a == only6Mark {
					m.Only6 = append(m.Only6, n)
				} else if p, err := netip.ParsePrefix(a); err == nil {
					m.Subnets = append(m.Subnets, NetAddress{Network: n, Address: p})
				}
			}
		}
		if len(f) >= 8 {
			for _, t := range strings.Fields(f[7]) {
				if v, a, ok := strings.Cut(t, "/"); ok && v != "" && a != "" {
					m.Links = append(m.Links, Link{Viewer: v, Alias: a})
				}
			}
		}
		m.Router, _ = strconv.Atoi(f[3])
		for _, p := range strings.Split(f[2], ",") {
			k, v, ok := strings.Cut(p, "=")
			if !ok {
				continue
			}
			if h, err := strconv.Atoi(v); err == nil {
				m.Ports[k] = h
			}
		}
		out = append(out, m)
	}
	return out
}

// Find returns the member host names, as DNS compares names: in any case,
// with or without a final dot; or the member one of whose addresses host
// is. A
// name no member answers to may be an alias a link gives one, for
// whichever member makes the link.
func Find(ms []Member, host string) (Member, bool) {
	name := strings.TrimSuffix(host, ".")
	for _, m := range ms {
		if m.Has(host) {
			return m, true
		}
		for _, n := range m.Names {
			if strings.EqualFold(n, name) {
				return m, true
			}
		}
	}
	for _, m := range ms {
		for _, l := range m.Links {
			if strings.EqualFold(l.Alias, name) {
				return m, true
			}
		}
	}
	return Member{}, false
}

// Sees reports whether the member self reaches the member to: they share
// a network, or self has a link to it.
func Sees(self, to Member) bool {
	if Share(self, to) {
		return true
	}
	for _, l := range to.Links {
		if l.Viewer == self.Address {
			return true
		}
	}
	return false
}

// Share reports whether two members are on a network together.
func Share(a, b Member) bool {
	for _, n := range a.Networks {
		for _, o := range b.Networks {
			if n == o {
				return true
			}
		}
	}
	return false
}

// Lookup finds the member host names as the member at self sees it, as the
// container library resolves names: a member self sees that answers to
// host, as DNS compares names, or an alias one of self's links gives it, or
// the member one of whose addresses host is, when self sees it.
func Lookup(ms []Member, self, host string) (Member, bool) {
	me, ok := Find(ms, self)
	if !ok || me.Address != self {
		return Member{}, false
	}
	name := strings.TrimSuffix(host, ".")
	for _, m := range ms {
		if !Sees(me, m) && m.Address != me.Address {
			continue
		}
		if m.Has(host) {
			return m, true
		}
		for _, n := range m.Names {
			if strings.EqualFold(n, name) {
				return m, true
			}
		}
		for _, l := range m.Links {
			if l.Viewer == me.Address && strings.EqualFold(l.Alias, name) {
				return m, true
			}
		}
	}
	return Member{}, false
}

// Complete is the name host stands for as the member at self resolves it,
// before aliases: the first of the names its search domains make of host,
// in the order its resolver tries them, that a member it sees answers to,
// as Lookup finds them; host where none does. A name with a final dot, or
// as many dots as the member's ndots, is tried as it is first.
func Complete(ms []Member, self, host string) string {
	me, ok := Find(ms, self)
	if !ok || me.Address != self || len(me.Search) == 0 || strings.HasSuffix(host, ".") || host == "" {
		return host
	}
	answers := func(name string) bool {
		_, ok := Lookup(ms, self, name)
		return ok
	}
	ndots := max(me.Ndots, 1)
	first := strings.Count(host, ".") >= ndots
	if first && answers(host) {
		return host
	}
	for _, d := range me.Search {
		if name := host + "." + strings.TrimSuffix(d, "."); answers(name) {
			return name
		}
	}
	return host
}

// maxAliases is how many aliases a name is followed through, as a
// resolver bounds a chain of CNAME records.
const maxAliases = 8

// Unalias is the name host stands for as the member at self resolves it:
// the name the alias host is, as Lookup finds it, followed through aliases
// to a name that is none; host itself where it is no alias, or where the
// chain runs past maxAliases.
func Unalias(ms []Member, self, host string) string {
	name := host
	for range maxAliases {
		m, ok := Lookup(ms, self, name)
		if !ok || m.Alias == "" {
			return name
		}
		name = m.Alias
	}
	return host
}
