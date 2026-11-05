// Package members reads and writes a stack's network file: a line per
// member of the stack, of its member address, the names it answers to, the
// host ports behind its ports, its router's port, the networks it is on,
// whether it belongs to another stack, that stack, and the aliases links
// give it, tab-separated. The engine writes it;
// the container library, the container's router and the stack's egress
// proxy read it.
package members

import (
	"bytes"
	"fmt"
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
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%s\t%d\t%s\t%s\n", m.Address, strings.Join(m.Names, " "), strings.Join(ports, ","), m.Router, strings.Join(m.Networks, " "), foreign, m.Stack, strings.Join(links, " "))
	}
	return b.Bytes()
}

// Parse reads a network file, leaving out lines it cannot read.
func Parse(b []byte) []Member {
	var out []Member
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 6 || len(f) > 8 || f[0] == "" {
			continue
		}
		m := Member{Address: f[0], Names: strings.Fields(f[1]), Ports: map[string]int{}, Networks: strings.Fields(f[4]), Foreign: f[5] == "1"}
		if len(f) >= 7 {
			m.Stack = f[6]
		}
		if len(f) == 8 {
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
// with or without a final dot; or the member whose address host is.
func Find(ms []Member, host string) (Member, bool) {
	name := strings.TrimSuffix(host, ".")
	for _, m := range ms {
		if m.Address == host {
			return m, true
		}
		for _, n := range m.Names {
			if strings.EqualFold(n, name) {
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
