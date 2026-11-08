package engine

import (
	"net"
	"net/netip"
	"sort"
	"strconv"

	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/portproxy"
	"github.com/e6qu/fun-with-sandboxes/internal/shim"
)

// published is one host binding a run's shim makes for a container port.
type published struct {
	port    network.Port
	binding shim.Binding
	// report is the host port inspect shows, when not the one bound: a
	// nested engine's port in its privileged container.
	report int
}

// publishPlan lists the host bindings the container's ports ask for, TCP
// or UDP, for its shim to bind. A host port range takes the first port of
// it the host can bind.
func (e *Engine) publishPlan(c *Container) ([]published, error) {
	bindings := network.PortMap{}
	for p, bs := range c.rec.HostConfig.PortBindings {
		bindings[p] = bs
	}
	if c.rec.HostConfig.PublishAllPorts {
		for p := range c.rec.Config.ExposedPorts {
			if _, ok := bindings[p]; !ok && (p.Proto() == network.TCP || p.Proto() == network.UDP) {
				bindings[p] = []network.PortBinding{{}}
			}
		}
	}
	keys := make([]network.Port, 0, len(bindings))
	for p := range bindings {
		keys = append(keys, p)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	var plan []published
	for _, p := range keys {
		target, ok := c.rec.Ports[p.String()]
		if !ok {
			continue
		}
		for _, b := range bindings[p] {
			ip := b.HostIP
			if !ip.IsValid() {
				ip = netip.IPv4Unspecified()
			}
			lo, hi, err := portproxy.HostPortRange(b.HostPort)
			if err != nil {
				return nil, invalid("invalid host port %q: %v", b.HostPort, err)
			}
			upstream := net.JoinHostPort("127.0.0.1", strconv.Itoa(target))
			if n := e.cfg.Nested; n != nil {
				pub, err := nestedBinding(n, p, ip, lo, hi, upstream)
				if err != nil {
					return nil, err
				}
				plan = append(plan, pub)
				continue
			}
			plan = append(plan, published{port: p, binding: shim.Binding{
				Proto: string(p.Proto()), IP: ip, Lo: lo, Hi: hi, Upstream: upstream,
			}})
		}
	}
	return plan, nil
}

// nestedBinding publishes a nested engine's container port in the
// privileged container, as dockerd publishes in its own network namespace:
// on that container's port's host port when it has one, else on the Unix
// socket its processes reach the port at.
func nestedBinding(n *Nested, p network.Port, ip netip.Addr, lo, hi int, upstream string) (published, error) {
	if p.Proto() != network.TCP {
		return published{}, unsupported("publishing %s is not supported in a privileged container: its engine publishes TCP ports only", p)
	}
	hostPort, dir, err := n.Port(lo)
	if err != nil {
		return published{}, err
	}
	if dir == "" {
		return published{port: p, report: lo, binding: shim.Binding{
			Proto: "tcp", IP: netip.MustParseAddr("127.0.0.1"), Lo: hostPort, Hi: hostPort, Upstream: upstream,
		}}, nil
	}
	return published{port: p, binding: shim.Binding{Proto: "tcp", IP: ip, Lo: lo, Hi: hi, Upstream: upstream, Dir: dir}}, nil
}
