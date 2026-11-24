package engine

import (
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

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
				ip = e.hostBinding(c)
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
// socket its processes reach the port at, a stream socket for a TCP port
// and a datagram socket for a UDP one.
func nestedBinding(n *Nested, p network.Port, ip netip.Addr, lo, hi int, upstream string) (published, error) {
	proto := string(p.Proto())
	if p.Proto() != network.TCP && p.Proto() != network.UDP {
		return published{}, unsupported("publishing %s is not supported in a privileged container: its engine publishes TCP and UDP ports only", p)
	}
	hostPort, dir, err := n.Port(proto, lo)
	if err != nil {
		return published{}, err
	}
	if dir == "" {
		return published{port: p, report: lo, binding: shim.Binding{
			Proto: proto, IP: netip.MustParseAddr("127.0.0.1"), Lo: hostPort, Hi: hostPort, Upstream: upstream,
		}}, nil
	}
	return published{port: p, binding: shim.Binding{Proto: proto, IP: ip, Lo: lo, Hi: hi, Upstream: upstream, Dir: dir}}, nil
}

// ingressAllowLabel names the sources a container's published ports take
// connections and datagrams from: comma-separated addresses and CIDR
// prefixes. Docker has no such setting.
const ingressAllowLabel = "fws.ingress.allow"

// ingressAllow reads a container's ingress allow list, nil for none.
func ingressAllow(labels map[string]string) ([]netip.Prefix, error) {
	v, ok := labels[ingressAllowLabel]
	if !ok {
		return nil, nil
	}
	var out []netip.Prefix
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		var p netip.Prefix
		var err error
		if strings.Contains(e, "/") {
			p, err = netip.ParsePrefix(e)
		} else {
			var a netip.Addr
			if a, err = netip.ParseAddr(e); err == nil {
				p = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
			}
		}
		if err != nil {
			return nil, invalid("label %s: %q is not an address or prefix", ingressAllowLabel, e)
		}
		out = append(out, p.Masked())
	}
	if len(out) == 0 {
		return nil, invalid("label %s names nothing", ingressAllowLabel)
	}
	return out, nil
}

// hostBinding is the host address a container's port publishes on when
// its binding names none: its network's host_binding_ipv4, as Docker's
// bridge takes it, or every address.
func (e *Engine) hostBinding(c *Container) netip.Addr {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := make([]string, 0, len(c.rec.Networks))
	for n := range c.rec.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if nw, err := e.networkLocked(n); err == nil {
			if v := nw.Options[optHostBinding]; v != "" {
				if a, err := netip.ParseAddr(v); err == nil {
					return a
				}
			}
		}
	}
	return netip.IPv4Unspecified()
}
