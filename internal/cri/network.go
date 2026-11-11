package cri

import (
	"fmt"
	"net/netip"
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

// podSubnet is the IPv4 subnet of a pod CIDR the kubelet sends: one CIDR,
// or a dual-stack pair separated by a comma, of which the pod network
// takes the IPv4 one, as the engine reaches every member at an IPv4
// address.
func podSubnet(cidrs string) (netip.Prefix, error) {
	for _, c := range strings.Split(cidrs, ",") {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("pod CIDR %q: %v", cidrs, err)
		}
		if p.Addr().Is4() {
			if p.Masked() != p {
				return netip.Prefix{}, fmt.Errorf("pod CIDR %s is not a network address: it should be %s", p, p.Masked())
			}
			return p, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("pod CIDR %q holds no IPv4 subnet", cidrs)
}

// ensurePodNetwork makes the pod network on the node's pod CIDR. A pod
// network of another subnet that no sandbox is on gives way to it; one
// that sandboxes are on stays, as their addresses are its.
func (s *Service) ensurePodNetwork() error {
	s.netMu.Lock()
	defer s.netMu.Unlock()
	want := s.podCIDR
	if !want.IsValid() {
		want = netip.MustParsePrefix(defaultPodCIDR)
	}
	in, err := s.e.NetworkInspect(PodNetwork)
	switch {
	case err == nil:
		for _, c := range in.IPAM.Config {
			if c.Subnet == want {
				return nil
			}
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
	_, err = s.e.NetworkCreate(engine.NetworkCreate{
		Name:   PodNetwork,
		IPAM:   &network.IPAM{Config: []network.IPAMConfig{{Subnet: want}}},
		Labels: map[string]string{kindLabel: "pods"},
	})
	return err
}
