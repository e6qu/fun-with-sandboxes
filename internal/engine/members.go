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

// assignAddressLocked returns the lowest member address the stack has free.
func (e *Engine) assignAddressLocked(stack string) (string, error) {
	used := map[string]bool{}
	for _, o := range e.containers {
		if o.rec.Stack == stack {
			used[o.rec.Address] = true
		}
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

func onNetwork(c *Container) bool { return c.rec.HostConfig.NetworkMode != "none" }

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

// writeNetworkLocked writes the stack's network file: a line per member on
// the network, of its address, its names and its ports, tab-separated.
func (e *Engine) writeNetworkLocked(stack string) error {
	path, err := e.networkFile(stack)
	if err != nil {
		return err
	}
	var members []*Container
	for _, o := range e.containers {
		if o.rec.Stack == stack && o.rec.Address != "" && onNetwork(o) {
			members = append(members, o)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].rec.Address < members[j].rec.Address })
	var ms []stackmembers.Member
	for _, m := range members {
		ports := map[string]int{}
		for k, v := range m.rec.Ports {
			if _, proto, _ := strings.Cut(k, "/"); proto == "tcp" || proto == "udp" {
				ports[k] = v
			}
		}
		ms = append(ms, stackmembers.Member{Address: m.rec.Address, Names: memberNames(m), Ports: ports})
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
