// Package members reads and writes a stack's network file: a line per
// member of the stack, of its member address, the names it answers to, the
// host ports behind its ports, its router's port, the networks it is on,
// whether it belongs to another stack, that stack, the aliases links give
// it, and its addresses on networks with subnets of their own,
// tab-separated. The engine writes it;
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
	// is sent from, so that its replies may return.
	Stack string
	// Links are the names the member answers to for one member alone, as
	// a container's links name others; a legacy link also lets the member
	// that makes it reach this one where they share no network.
	Links []Link
	// Subnets are the member's addresses on the networks it is on that
	// have subnets of their own, IPv4 and IPv6, each with its subnet's
	// length; on every other network the member is at Address.
	Subnets []NetAddress
}

// NetAddress is a member's address on a network with a subnet.
type NetAddress struct {
	Network string
	Address netip.Prefix
}

// Has reports whether a is one of the member's addresses.
func (m Member) Has(a string) bool {
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

// On is the member's IPv4 address on network.
func (m Member) On(network string) string {
	for _, s := range m.Subnets {
		if s.Network == network && s.Address.Addr().Is4() {
			return s.Address.Addr().String()
		}
	}
	return m.Address
}

// SeenBy is the member's address as viewer reaches it: on the first of
// its networks the viewer is on too, else its member address.
func (m Member) SeenBy(viewer Member) string {
	for _, n := range m.Networks {
		if slices.Contains(viewer.Networks, n) {
			return m.On(n)
		}
	}
	return m.Address
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
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%s\t%d\t%s\t%s", m.Address, strings.Join(m.Names, " "), strings.Join(ports, ","), m.Router, strings.Join(m.Networks, " "), foreign, m.Stack, strings.Join(links, " "))
		if len(m.Subnets) > 0 {
			subnets := make([]string, len(m.Subnets))
			for i, s := range m.Subnets {
				subnets[i] = s.Network + "=" + s.Address.String()
			}
			fmt.Fprintf(&b, "\t%s", strings.Join(subnets, " "))
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
		if len(f) < 6 || len(f) > 9 || f[0] == "" {
			continue
		}
		m := Member{Address: f[0], Names: strings.Fields(f[1]), Ports: map[string]int{}, Networks: strings.Fields(f[4]), Foreign: f[5] == "1"}
		if len(f) >= 7 {
			m.Stack = f[6]
		}
		if len(f) == 9 {
			for _, t := range strings.Fields(f[8]) {
				n, a, ok := strings.Cut(t, "=")
				if p, err := netip.ParsePrefix(a); ok && n != "" && err == nil {
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
