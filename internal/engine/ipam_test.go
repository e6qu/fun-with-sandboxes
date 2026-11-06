package engine

import (
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/alloc"
)

func ipamEngine(t *testing.T) *Engine {
	t.Helper()
	a, err := alloc.Open(alloc.Config{Root: t.TempDir(), PortBase: 30000, PortLimit: 31000, BlockSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{alloc: a, containers: map[string]*Container{}, networks: map[string]*Network{}}
}

func subnetConfig(subnet, gateway, ipRange string) *network.IPAM {
	c := network.IPAMConfig{Subnet: netip.MustParsePrefix(subnet)}
	if gateway != "" {
		c.Gateway = netip.MustParseAddr(gateway)
	}
	if ipRange != "" {
		c.IPRange = netip.MustParsePrefix(ipRange)
	}
	return &network.IPAM{Config: []network.IPAMConfig{c}}
}

// A network takes one IPv4 subnet of the default driver outside loopback
// and every other network's subnet, as Docker's default IPAM driver does.
func TestIPAMConfigurations(t *testing.T) {
	e := ipamEngine(t)
	n := &Network{ID: "n1", Name: "front", Driver: "bridge"}
	if err := e.checkIPAMLocked(n, subnetConfig("172.28.0.0/16", "", "172.28.5.0/24"), nil); err != nil {
		t.Fatal(err)
	}
	if n.Subnet != "172.28.0.0/16" || n.Gateway != "172.28.0.1" || n.IPRange != "172.28.5.0/24" {
		t.Fatalf("recorded %+v", n)
	}
	if got := n.ipamConfig(); len(got) != 1 || got[0].Gateway.String() != "172.28.0.1" || got[0].IPRange.String() != "172.28.5.0/24" {
		t.Errorf("reported %+v", got)
	}
	e.networks[n.ID] = n
	yes := true
	for name, c := range map[string]struct {
		ipam *network.IPAM
		ipv6 *bool
		kind Kind
	}{
		"an overlapping subnet":      {subnetConfig("172.28.1.0/24", "", ""), nil, KindForbidden},
		"a loopback subnet":          {subnetConfig("127.99.0.0/16", "", ""), nil, KindUnsupported},
		"host bits":                  {subnetConfig("10.1.0.1/24", "", ""), nil, KindInvalid},
		"no host addresses":          {subnetConfig("10.1.0.0/31", "", ""), nil, KindInvalid},
		"a gateway outside":          {subnetConfig("10.1.0.0/24", "10.2.0.1", ""), nil, KindInvalid},
		"a range outside":            {subnetConfig("10.1.0.0/24", "", "10.2.0.0/25"), nil, KindInvalid},
		"an IPv6 subnet":             {subnetConfig("fd00::/64", "", ""), nil, KindUnsupported},
		"IPv6":                       {nil, &yes, KindUnsupported},
		"another driver":             {&network.IPAM{Driver: "dhcp"}, nil, KindUnsupported},
		"two subnets":                {&network.IPAM{Config: append(subnetConfig("10.1.0.0/24", "", "").Config, subnetConfig("10.2.0.0/24", "", "").Config...)}, nil, KindUnsupported},
		"a configuration, no subnet": {&network.IPAM{Config: []network.IPAMConfig{{Gateway: netip.MustParseAddr("10.1.0.1")}}}, nil, KindInvalid},
	} {
		err := e.checkIPAMLocked(&Network{Name: "x"}, c.ipam, c.ipv6)
		if err == nil || KindOf(err) != c.kind {
			t.Errorf("%s: %v, want kind %v", name, err, c.kind)
		}
	}
	if got := (&Network{Driver: "bridge"}).ipamConfig(); len(got) != 1 || got[0].Subnet.String() != memberSubnet || got[0].Gateway.String() != memberGateway {
		t.Errorf("a network without a subnet reports %+v", got)
	}
}

// Containers take the lowest free address of the range, or the one they
// name in the subnet, and none on a network without a subnet.
func TestNetworkAddresses(t *testing.T) {
	e := ipamEngine(t)
	n := &Network{ID: "n1", Name: "front", Driver: "bridge"}
	ipam := subnetConfig("10.5.0.0/24", "10.5.0.1", "")
	ipam.Config[0].AuxAddress = map[string]netip.Addr{"router": netip.MustParseAddr("10.5.0.2")}
	if err := e.checkIPAMLocked(n, ipam, nil); err != nil {
		t.Fatal(err)
	}
	e.networks[n.ID] = n
	join := func(id, static string) (*network.EndpointSettings, error) {
		s := &network.EndpointSettings{}
		if static != "" {
			s.IPAMConfig = &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr(static)}
		}
		if err := e.assignNetAddressLocked(n, id, s); err != nil {
			return nil, err
		}
		e.containers[id] = &Container{rec: record{ID: id, Address: "127.88.0.9", Networks: map[string]*network.EndpointSettings{n.Name: s}}}
		return s, nil
	}
	a, err := join("a", "")
	if err != nil || a.IPAddress.String() != "10.5.0.3" || a.IPPrefixLen != 24 || a.Gateway.String() != "10.5.0.1" || a.MacAddress.String() != "02:42:0a:05:00:03" {
		t.Fatalf("first container: %+v, %v", a, err)
	}
	if b, err := join("b", "10.5.0.200"); err != nil || b.IPAddress.String() != "10.5.0.200" {
		t.Fatalf("static address: %+v, %v", b, err)
	}
	if c, err := join("c", ""); err != nil || c.IPAddress.String() != "10.5.0.4" {
		t.Fatalf("third container: %+v, %v", c, err)
	}
	for static, kind := range map[string]Kind{"10.5.0.200": KindForbidden, "10.5.0.1": KindForbidden, "10.5.0.2": KindForbidden, "10.6.0.9": KindInvalid} {
		if _, err := join("d", static); err == nil || KindOf(err) != kind {
			t.Errorf("static %s: %v, want kind %v", static, err, kind)
		}
	}
	plain := &Network{ID: "n2", Name: "plain", Driver: "bridge"}
	s := &network.EndpointSettings{IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.5.0.50")}}
	if err := e.assignNetAddressLocked(plain, "e", s); err == nil || KindOf(err) != KindInvalid {
		t.Errorf("a static address on a network without a subnet: %v", err)
	}
	// The container reports its subnet address there, and its member
	// address on any other network.
	c := e.containers["a"]
	c.rec.Networks["plain"] = &network.EndpointSettings{}
	if es := c.endpoint("front"); es.IPAddress.String() != "10.5.0.3" {
		t.Errorf("endpoint on front: %+v", es)
	}
	if es := c.endpoint("plain"); es.IPAddress.String() != "127.88.0.9" || es.IPPrefixLen != 16 || es.Gateway.String() != memberGateway {
		t.Errorf("endpoint on plain: %+v", es)
	}
}

// The network file names each member's address on a network with a
// subnet.
func TestNetworkFileSubnets(t *testing.T) {
	e := ipamEngine(t)
	add := func(id, address, ip string) {
		nets := map[string]*network.EndpointSettings{"proj_default": {}, "proj_lan": {IPAddress: netip.MustParseAddr(ip), IPPrefixLen: 24}}
		e.containers[id] = &Container{rec: record{ID: id, Name: "proj-" + id, Stack: "proj", Address: address, Networks: nets}}
	}
	add("a", "127.88.0.2", "10.5.0.3")
	add("b", "127.88.0.3", "10.5.0.4")
	if err := e.writeNetworkLocked("proj"); err != nil {
		t.Fatal(err)
	}
	path, _ := e.networkFile("proj")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\tproj_default proj_lan\t0\t\t\tproj_lan=10.5.0.3/24\n") || !strings.Contains(string(b), "proj_lan=10.5.0.4/24\n") {
		t.Errorf("network file:\n%s", b)
	}
}
