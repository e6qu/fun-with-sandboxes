package engine

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"sync"

	stackmembers "github.com/e6qu/fun-with-sandboxes/internal/members"
	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// Engines nested in privileged containers that share a network are a
// cluster's nodes, and each one's pods reach the others' at their
// addresses, as a cluster's pod network carries them between nodes. A
// nested engine publishes its members on networks with subnets of their
// own to the engine it is nested in, which hands them to the nested
// engines of the containers its container shares a network with: each
// lists them in its network files as other stacks' members on the
// networks of the same names, reached through their routers, which
// listen on the host's loopback as every router does.
//
// A nested engine publishes while it holds its own lock, and the outer
// engine refreshes the others from a goroutine of its own, taking no
// nested engine's lock while it holds one of its own.

// nodePeers holds what each nested engine published, by its container's ID,
// and the user-defined networks each such container is on.
type nodePeers struct {
	mu      sync.Mutex
	members map[string][]stackmembers.Member
	nets    map[string][]string
}

// publishPeer records the members c's nested engine published and hands
// them to the other nested engines.
func (e *Engine) publishPeer(c *Container, ms []stackmembers.Member) {
	e.nodes.mu.Lock()
	if e.nodes.members == nil {
		e.nodes.members = map[string][]stackmembers.Member{}
	}
	e.nodes.members[c.rec.ID] = ms
	e.nodes.mu.Unlock()
	go e.refreshPeers(c.rec.ID)
}

// dropPeer forgets c's nested engine's members, as it closes.
func (e *Engine) dropPeer(c *Container) {
	e.nodes.mu.Lock()
	_, had := e.nodes.members[c.rec.ID]
	delete(e.nodes.members, c.rec.ID)
	e.nodes.mu.Unlock()
	if had {
		go e.refreshPeers(c.rec.ID)
	}
}

// refreshPeers notes which networks each nested engine's container is on
// and has every nested engine but except's rewrite its network files.
func (e *Engine) refreshPeers(except string) {
	e.nestedMu.Lock()
	nested := make(map[string]*nestedEngine, len(e.nested))
	for id, n := range e.nested {
		nested[id] = n
	}
	e.nestedMu.Unlock()
	nets := map[string][]string{}
	e.mu.Lock()
	for id := range nested {
		if c := e.containers[id]; c != nil {
			nets[id] = userNetworks(c)
		}
	}
	e.mu.Unlock()
	e.nodes.mu.Lock()
	e.nodes.nets = nets
	e.nodes.mu.Unlock()
	for id, n := range nested {
		if id != except {
			n.e.rewriteNetworks()
		}
	}
}

// peersOf are the members other nested engines published whose
// containers share a network with container id's, as other stacks'.
func (e *Engine) peersOf(id string) []stackmembers.Member {
	e.nodes.mu.Lock()
	defer e.nodes.mu.Unlock()
	mine := e.nodes.nets[id]
	ids := make([]string, 0, len(e.nodes.members))
	for o := range e.nodes.members {
		if o == id {
			continue
		}
		for _, n := range e.nodes.nets[o] {
			if slices.Contains(mine, n) {
				ids = append(ids, o)
				break
			}
		}
	}
	sort.Strings(ids)
	var out []stackmembers.Member
	for _, o := range ids {
		for _, m := range e.nodes.members[o] {
			m.Foreign, m.Stack = true, "peer-"+o[:12]
			out = append(out, m)
		}
	}
	return out
}

// dialPeer connects to port ap.Port of the member at ap.Addr that another
// nested engine published, one c's container shares a network with.
func (e *Engine) dialPeer(c *Container, ap netip.AddrPort) (net.Conn, error) {
	for _, m := range e.peersOf(c.rec.ID) {
		if !m.Has(ap.Addr().String()) {
			continue
		}
		if hp, ok := m.Ports[fmt.Sprintf("%d/tcp", ap.Port())]; ok {
			return net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
		}
		if m.Router != 0 {
			return router.Dial(net.JoinHostPort("127.0.0.1", strconv.Itoa(m.Router)), int(ap.Port()))
		}
		return nil, fmt.Errorf("%s does not listen on port %d", ap.Addr(), ap.Port())
	}
	return nil, fmt.Errorf("no member at %s shares a network with %s", ap.Addr(), c.rec.Name)
}

// crossMembersLocked are what a nested engine publishes: its members on
// networks with subnets of their own, at their addresses there, which are
// unique on a cluster's pod network as member addresses are not across
// engines.
func (e *Engine) crossMembersLocked() []stackmembers.Member {
	var out []stackmembers.Member
	for _, o := range e.containers {
		if o.rec.Address == "" || !onNetwork(o) || !o.isRunning() {
			continue
		}
		m := stackmembers.Member{Ports: map[string]int{}, Router: o.rec.RouterPort}
		for _, n := range userNetworks(o) {
			es := o.rec.Networks[n]
			if es == nil || !es.IPAddress.IsValid() {
				continue
			}
			m.Networks = append(m.Networks, n)
			m.Subnets = append(m.Subnets, stackmembers.NetAddress{Network: n, Address: netip.PrefixFrom(es.IPAddress, es.IPPrefixLen)})
			if m.Address == "" {
				m.Address = es.IPAddress.String()
			}
		}
		if m.Address == "" {
			continue
		}
		for k, v := range o.rec.Ports {
			m.Ports[k] = v
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// publishLocked hands a nested engine's members on networks with subnets
// to the engine it is nested in, when they changed.
func (e *Engine) publishLocked() {
	n := e.cfg.Nested
	if n == nil || n.Publish == nil {
		return
	}
	ms := e.crossMembersLocked()
	b := stackmembers.Format(ms)
	if bytes.Equal(b, e.published) && e.published != nil {
		return
	}
	e.published = b
	n.Publish(ms)
}

// rewriteNetworks rewrites the network file of every stack, as the
// members other engines publish change.
func (e *Engine) rewriteNetworks() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.writeAllNetworksLocked()
}

func (e *Engine) writeAllNetworksLocked() error {
	stacks := map[string]bool{}
	for _, c := range e.containers {
		stacks[c.rec.Stack] = true
	}
	for st := range stacks {
		if err := e.writeStackNetworkLocked(st); err != nil {
			return err
		}
	}
	return nil
}
