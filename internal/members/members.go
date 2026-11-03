// Package members reads and writes a stack's network file: a line per
// member of the stack, of its member address, the names it answers to and
// the host ports behind its ports, tab-separated. The engine writes it; the
// container library and the stack's egress proxy read it.
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
		fmt.Fprintf(&b, "%s\t%s\t%s\n", m.Address, strings.Join(m.Names, " "), strings.Join(ports, ","))
	}
	return b.Bytes()
}

// Parse reads a network file, leaving out lines it cannot read.
func Parse(b []byte) []Member {
	var out []Member
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || f[0] == "" {
			continue
		}
		m := Member{Address: f[0], Names: strings.Fields(f[1]), Ports: map[string]int{}}
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
