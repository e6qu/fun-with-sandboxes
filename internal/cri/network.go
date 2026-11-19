package cri

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// PodNetwork is the user-defined network every pod sandbox not on its
// node's network joins, as a cluster's pod network carries every pod:
// pods reach one another at their addresses on it, through their routers.
const PodNetwork = "fws-pods"

// defaultPodCIDR is the pod network's subnet until the kubelet sends its
// node's pod CIDR, as a runtime driven by crictl alone never does; it is
// kind's pod subnet.
const defaultPodCIDR = "10.244.0.0/16"

// podSubnets are the subnets of a pod CIDR the kubelet sends: one CIDR,
// IPv4 or IPv6, or a dual-stack pair separated by a comma, IPv4 first.
// The pod network takes them all: a pod has an address of each family, on
// an IPv6-only network an IPv6 one alone.
func podSubnets(cidrs string) ([]netip.Prefix, error) {
	var v4, v6 []netip.Prefix
	for _, c := range strings.Split(cidrs, ",") {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			return nil, fmt.Errorf("pod CIDR %q: %v", cidrs, err)
		}
		if p.Masked() != p {
			return nil, fmt.Errorf("pod CIDR %s is not a network address: it should be %s", p, p.Masked())
		}
		if p.Addr().Is4() {
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	if len(v4) > 1 || len(v6) > 1 {
		return nil, fmt.Errorf("pod CIDR %q holds more than one subnet of a family", cidrs)
	}
	return append(v4, v6...), nil
}

// ensurePodNetwork makes the pod network on the node's pod CIDR, dual-stack
// where it holds an IPv4 and an IPv6 subnet, IPv6-only where it holds an
// IPv6 one alone. A pod network of other subnets that no sandbox is on
// gives way to it; one that sandboxes are on stays, as their addresses are
// its.
func (s *Service) ensurePodNetwork() error {
	s.netMu.Lock()
	defer s.netMu.Unlock()
	want := s.podCIDRs
	if len(want) == 0 {
		want = []netip.Prefix{netip.MustParsePrefix(defaultPodCIDR)}
	}
	in, err := s.e.NetworkInspect(PodNetwork)
	switch {
	case err == nil:
		var have []netip.Prefix
		for _, c := range in.IPAM.Config {
			have = append(have, c.Subnet)
		}
		if sameSubnets(have, want) {
			return nil
		}
		if err := s.e.NetworkRemove(PodNetwork); err != nil {
			if engine.KindOf(err) == engine.KindConflict {
				return nil
			}
			return err
		}
	case engine.KindOf(err) != engine.KindNotFound:
		return err
	}
	var configs []network.IPAMConfig
	has4, has6 := false, false
	for _, p := range want {
		configs = append(configs, network.IPAMConfig{Subnet: p})
		has4, has6 = has4 || p.Addr().Is4(), has6 || p.Addr().Is6()
	}
	_, err = s.e.NetworkCreate(engine.NetworkCreate{
		Name:       PodNetwork,
		EnableIPv4: &has4,
		EnableIPv6: &has6,
		IPAM:       &network.IPAM{Config: configs},
		Labels:     map[string]string{kindLabel: "pods"},
	})
	return err
}

// sameSubnets reports whether a and b hold the same subnets, in any order.
func sameSubnets(a, b []netip.Prefix) bool {
	if len(a) != len(b) {
		return false
	}
	for _, p := range a {
		if !slices.Contains(b, p) {
			return false
		}
	}
	return true
}
