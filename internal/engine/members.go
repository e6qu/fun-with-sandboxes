package engine

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	stackmembers "github.com/e6qu/fun-with-sandboxes/internal/members"
)

// A stack's members reach one another by name, as containers on a Docker
// network do, through the container library: each member has an address
// of its own in memberBlock, which the kernel never sees, and the stack's
// network file names each member's address, the names it answers to and
// the host ports behind its ports.
const memberBlock = "127.88."

// assignAddressLocked returns the lowest member address no container holds:
// stacks that share a network see one another's members, so an address
// names one member across every stack.
func (e *Engine) assignAddressLocked(stack string) (string, error) {
	used := map[string]bool{}
	for _, o := range e.containers {
		used[o.rec.Address] = true
	}
	for i := 2; i < 1<<16-1; i++ {
		a := fmt.Sprintf("%s%d.%d", memberBlock, i>>8, i&255)
		if !used[a] {
			return a, nil
		}
	}
	return "", fmt.Errorf("stack %s has no member address free", stack)
}

func (e *Engine) networkFile(stack string) (string, error) {
	dir, err := e.alloc.Dir(stack)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "network"), nil
}

// onNetwork reports whether c has a network endpoint of its own: not on no
// network, and not on another container's.
func onNetwork(c *Container) bool {
	m := string(c.rec.HostConfig.NetworkMode)
	return m != "none" && m != "host" && !strings.HasPrefix(m, "container:")
}

// hostNode is the privileged container an engine nested in it puts the
// containers on the host's network mode on, and the engine that runs it,
// or nil for an engine that is not nested, which refuses that mode.
func (e *Engine) hostNode() (*Engine, *Container) {
	if e.cfg.Nested == nil || e.cfg.Nested.Node == nil {
		return nil, nil
	}
	return e.cfg.Nested.Node()
}

// netOf is the container whose network c is on and the engine that runs
// it: netOwner's, or for a nested engine's container on the host's
// network mode, the privileged container the engine is nested in, whose
// ports, addresses and router its processes take as their own, as a
// process on a Linux host's network namespace shares the host's.
func (e *Engine) netOf(c *Container) (*Engine, *Container, error) {
	if c.rec.HostConfig.NetworkMode == "host" {
		if oe, node := e.hostNode(); node != nil {
			return oe, node, nil
		}
	}
	net, err := e.netOwner(c)
	return e, net, err
}

// netOwner is the container whose network c is on: c, or with network mode
// container:<id>, that container, which its record names by full ID.
func (e *Engine) netOwner(c *Container) (*Container, error) {
	id, ok := strings.CutPrefix(string(c.rec.HostConfig.NetworkMode), "container:")
	if !ok {
		return c, nil
	}
	return e.Container(id)
}

// memberNames are the names a member answers to: its container name, its
// service, its hostname and its network aliases.
func memberNames(c *Container) []string {
	names := []string{c.rec.Name, c.rec.Service, c.rec.Config.Hostname}
	nets := make([]string, 0, len(c.rec.Networks))
	for n := range c.rec.Networks {
		nets = append(nets, n)
	}
	sort.Strings(nets)
	for _, n := range nets {
		if s := c.rec.Networks[n]; s != nil {
			names = append(names, s.DNSNames...)
			names = append(names, s.Aliases...)
		}
	}
	var out []string
	for _, n := range uniqStrings(names) {
		if n != "" && !strings.ContainsAny(n, " \t\n") {
			out = append(out, n)
		}
	}
	return out
}

// userNetworks are the user-defined networks c is on: those another stack's
// containers may share with it.
func userNetworks(c *Container) []string {
	var out []string
	for n := range c.rec.Networks {
		if n != "bridge" && n != "none" && n != "host" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// writeNetworkLocked rewrites the network file of the stack and of every
// other stack: a member that comes or goes on a network stacks share
// changes the files of all of them.
func (e *Engine) writeNetworkLocked(stack string) error {
	stacks := map[string]bool{stack: true}
	for _, o := range e.containers {
		stacks[o.rec.Stack] = true
	}
	for s := range stacks {
		if err := e.writeStackNetworkLocked(s); err != nil {
			return err
		}
	}
	e.publishLocked()
	return nil
}

// writeStackNetworkLocked writes the stack's network file: a line per
// member on a network, of its address, its names, its ports, its router,
// its networks, whether it is another stack's, that stack, the aliases the
// stack's links give it, and its addresses on networks with subnets,
// tab-separated. Another stack's member
// is listed when it shares a user-defined network with one of the
// stack's, or one of the stack's links to it.
func (e *Engine) writeStackNetworkLocked(stack string) error {
	path, err := e.networkFile(stack)
	if err != nil {
		return err
	}
	var own, foreign []*Container
	shared := map[string]bool{}
	for _, o := range e.containers {
		if o.rec.Stack == stack && o.rec.Address != "" && onNetwork(o) {
			own = append(own, o)
			for _, n := range userNetworks(o) {
				shared[n] = true
			}
		}
	}
	taken := map[string]bool{}
	for _, o := range own {
		taken[o.rec.Address] = true
	}
	links := e.linkAliasesLocked(stack)
	for _, o := range e.containers {
		// Records of an older engine, which numbered each stack's members
		// alone, may give another stack's member one of this stack's
		// addresses, which the container could not tell apart.
		if o.rec.Stack == stack || o.rec.Address == "" || !onNetwork(o) || taken[o.rec.Address] {
			continue
		}
		if len(links[o]) > 0 {
			foreign = append(foreign, o)
			continue
		}
		for _, n := range userNetworks(o) {
			if shared[n] {
				foreign = append(foreign, o)
				break
			}
		}
	}
	nets := map[string]bool{}
	for _, o := range own {
		for n := range o.rec.Networks {
			nets[n] = true
		}
	}
	byAddress := func(cs []*Container) {
		sort.Slice(cs, func(i, j int) bool { return cs[i].rec.Address < cs[j].rec.Address })
	}
	byAddress(own)
	byAddress(foreign)
	var ms []stackmembers.Member
	for _, m := range append(own, foreign...) {
		ports := map[string]int{}
		for k, v := range m.rec.Ports {
			if _, proto, _ := strings.Cut(k, "/"); proto == "tcp" || proto == "udp" {
				ports[k] = v
			}
		}
		member := stackmembers.Member{Address: m.rec.Address, Names: memberNames(m), Ports: ports, Router: m.rec.RouterPort, Links: links[m]}
		if member.Foreign = m.rec.Stack != stack; member.Foreign {
			member.Networks, member.Stack = userNetworks(m), m.rec.Stack
		} else {
			for n := range m.rec.Networks {
				member.Networks = append(member.Networks, n)
			}
			sort.Strings(member.Networks)
		}
		for _, n := range member.Networks {
			if es := m.rec.Networks[n]; es != nil && es.IPAddress.IsValid() {
				member.Subnets = append(member.Subnets, stackmembers.NetAddress{Network: n, Address: netip.PrefixFrom(es.IPAddress, es.IPPrefixLen)})
			}
			if es := m.rec.Networks[n]; es != nil && es.GlobalIPv6Address.IsValid() {
				member.Subnets = append(member.Subnets, stackmembers.NetAddress{Network: n, Address: netip.PrefixFrom(es.GlobalIPv6Address, es.GlobalIPv6PrefixLen)})
			}
		}
		ms = append(ms, member)
	}
	ms = append(ms, e.serviceMembers(nets)...)
	// Other nodes' pods, on the networks of the same names.
	if n := e.cfg.Nested; n != nil && n.Peers != nil {
		for _, p := range n.Peers() {
			for _, pn := range p.Networks {
				if nets[pn] {
					ms = append(ms, p)
					break
				}
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, stackmembers.Format(ms), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// linkAliasesLocked returns, for each container the stack's containers
// link to, the aliases they know it by: legacy links on the default bridge
// network, which reach a container of any stack there, and links on a
// user-defined network, to the container on it that answers to the name.
func (e *Engine) linkAliasesLocked(stack string) map[*Container][]stackmembers.Link {
	out := map[*Container][]stackmembers.Link{}
	for _, v := range e.containers {
		if v.rec.Stack != stack || v.rec.Address == "" || !onNetwork(v) {
			continue
		}
		if v.rec.Networks["bridge"] != nil {
			for _, l := range v.rec.Links {
				if c, ok := e.containers[l.Child]; ok && c.rec.Address != "" && c.rec.Networks["bridge"] != nil {
					out[c] = append(out[c], stackmembers.Link{Viewer: v.rec.Address, Alias: l.Alias})
				}
			}
		}
		for n, pairs := range endpointLinks(v) {
			for _, p := range pairs {
				if c := e.answeringLocked(n, p[0], v); c != nil {
					out[c] = append(out[c], stackmembers.Link{Viewer: v.rec.Address, Alias: p[1]})
				}
			}
		}
	}
	return out
}

// answeringLocked finds the container other than v on network n that
// answers to name.
func (e *Engine) answeringLocked(n, name string, v *Container) *Container {
	ids := make([]string, 0, len(e.containers))
	for id := range e.containers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := e.containers[id]
		if c == v || c.rec.Address == "" || !onNetwork(c) || c.rec.Networks[n] == nil {
			continue
		}
		for _, m := range memberNames(c) {
			if strings.EqualFold(m, name) {
				return c
			}
		}
	}
	return nil
}
