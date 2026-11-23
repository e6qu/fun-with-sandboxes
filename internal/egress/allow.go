package egress

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AllowKey is the container label, and network driver option, that names
// the hosts a container may reach through its stack's egress proxy:
// comma-separated host names (example.com), wildcard domains
// (*.example.com, which match the domain's subdomains), IP addresses and
// CIDR prefixes, each with an optional :port. A container's label and its
// networks' options each restrict it: a destination must be on every
// list that applies.
const AllowKey = "fws.egress.allow"

// rule is one entry of an allow list.
type rule struct {
	host   string       // a name, or a wildcard's domain with its leading dot
	prefix netip.Prefix // an address or prefix
	port   int          // 0 for any
}

// ParseAllow reads an allow list.
func ParseAllow(s string) ([]rule, error) {
	var out []rule
	for _, e := range strings.Split(s, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		var r rule
		host := e
		if h, p, err := net.SplitHostPort(e); err == nil {
			n, perr := strconv.Atoi(p)
			if perr != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("egress allow entry %q: bad port", e)
			}
			host, r.port = h, n
		} else if i := strings.LastIndexByte(e, ':'); i > 0 && !strings.Contains(e[:i], ":") {
			return nil, fmt.Errorf("egress allow entry %q: bad port", e)
		}
		switch {
		case strings.Contains(host, "/"):
			p, err := netip.ParsePrefix(host)
			if err != nil {
				return nil, fmt.Errorf("egress allow entry %q: %v", e, err)
			}
			r.prefix = p.Masked()
		case net.ParseIP(host) != nil:
			a, _ := netip.ParseAddr(host)
			r.prefix = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		case strings.HasPrefix(host, "*."):
			r.host = strings.ToLower(host[1:])
		default:
			if host == "" || strings.ContainsAny(host, "* ") {
				return nil, fmt.Errorf("egress allow entry %q is not a host, *.domain, address or prefix", e)
			}
			r.host = strings.ToLower(strings.TrimSuffix(host, "."))
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("egress allow list %q names nothing", s)
	}
	return out, nil
}

// allows reports whether a destination, named host and resolving to ips,
// at port, is on the list: by its name, or by every address it resolves
// to.
func allows(rules []rule, host string, ips []net.IP, port int) bool {
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, r := range rules {
		if r.port != 0 && r.port != port || r.host == "" {
			continue
		}
		if name == r.host || strings.HasPrefix(r.host, ".") && strings.HasSuffix(name, r.host) {
			return true
		}
	}
	if len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			return false
		}
		in := false
		for _, r := range rules {
			if r.prefix.IsValid() && (r.port == 0 || r.port == port) && r.prefix.Contains(a.Unmap()) {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	return true
}

// AllowPath is where the engine keeps the allow lists of a stack's
// members, by member address, beside the stack's network file.
func AllowPath(networkFile string) string { return networkFile + ".allow" }

// allowTable reads the allow lists at path again when they change.
type allowTable struct {
	path   string
	mu     sync.Mutex
	mod    time.Time
	size   int64
	byAddr map[string][][]rule
}

// lists returns the lists that apply to the member at address, none when
// it may reach any host.
func (t *allowTable) lists(address string) [][]rule {
	if t == nil || address == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	info, err := os.Stat(t.path)
	if err != nil {
		t.byAddr, t.mod, t.size = nil, time.Time{}, 0
		return nil
	}
	if !info.ModTime().Equal(t.mod) || info.Size() != t.size {
		b, err := os.ReadFile(t.path)
		var specs map[string][]string
		if err != nil || json.Unmarshal(b, &specs) != nil {
			// An unreadable file allows nothing rather than everything.
			return [][]rule{nil}
		}
		next := map[string][][]rule{}
		for a, ss := range specs {
			for _, s := range ss {
				rs, err := ParseAllow(s)
				if err != nil {
					rs = nil
				}
				next[a] = append(next[a], rs)
			}
		}
		t.byAddr, t.mod, t.size = next, info.ModTime(), info.Size()
	}
	return t.byAddr[address]
}
