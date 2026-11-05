package engine

import (
	"fmt"
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
	return m != "none" && !strings.HasPrefix(m, "container:")
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
	return nil
}

// writeStackNetworkLocked writes the stack's network file: a line per
// member on a network, of its address, its names, its ports, its router,
// its networks, and whether it is another stack's, tab-separated. Another
// stack's member is listed when it shares a user-defined network with one
// of the stack's.
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
	for _, o := range e.containers {
		// Records of an older engine, which numbered each stack's members
		// alone, may give another stack's member one of this stack's
		// addresses, which the container could not tell apart.
		if o.rec.Stack == stack || o.rec.Address == "" || !onNetwork(o) || taken[o.rec.Address] {
			continue
		}
		for _, n := range userNetworks(o) {
			if shared[n] {
				foreign = append(foreign, o)
				break
			}
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
		member := stackmembers.Member{Address: m.rec.Address, Names: memberNames(m), Ports: ports, Router: m.rec.RouterPort}
		if member.Foreign = m.rec.Stack != stack; member.Foreign {
			member.Networks, member.Stack = userNetworks(m), m.rec.Stack
		} else {
			for n := range m.rec.Networks {
				member.Networks = append(member.Networks, n)
			}
			sort.Strings(member.Networks)
		}
		ms = append(ms, member)
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
