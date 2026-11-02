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
			plan = append(plan, published{port: p, binding: shim.Binding{
				Proto: string(p.Proto()), IP: ip, Lo: lo, Hi: hi,
				Upstream: net.JoinHostPort("127.0.0.1", strconv.Itoa(target)),
			}})
		}
	}
	return plan, nil
}
