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
	if err := e.checkIPAMLocked(n, subnetConfig("172.28.0.0/16", "", "172.28.5.0/24"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if n.Subnet != "172.28.0.0/16" || n.Gateway != "172.28.0.1" || n.IPRange != "172.28.5.0/24" {
		t.Fatalf("recorded %+v", n)
	}
	if got := n.ipamConfig(); len(got) != 1 || got[0].Gateway.String() != "172.28.0.1" || got[0].IPRange.String() != "172.28.5.0/24" {
		t.Errorf("reported %+v", got)
	}
	e.networks[n.ID] = n
	yes, no := true, false
	two := func(a, b string) *network.IPAM {
		return &network.IPAM{Config: append(subnetConfig(a, "", "").Config, subnetConfig(b, "", "").Config...)}
	}
	for name, c := range map[string]struct {
		ipam       *network.IPAM
		ipv4, ipv6 *bool
		kind       Kind
	}{
		"an overlapping subnet":         {subnetConfig("172.28.1.0/24", "", ""), nil, nil, KindForbidden},
		"a loopback subnet":             {subnetConfig("127.99.0.0/16", "", ""), nil, nil, KindUnsupported},
		"host bits":                     {subnetConfig("10.1.0.1/24", "", ""), nil, nil, KindInvalid},
		"no host addresses":             {subnetConfig("10.1.0.0/31", "", ""), nil, nil, KindInvalid},
		"a gateway outside":             {subnetConfig("10.1.0.0/24", "10.2.0.1", ""), nil, nil, KindInvalid},
		"a range outside":               {subnetConfig("10.1.0.0/24", "", "10.2.0.0/25"), nil, nil, KindInvalid},
		"an IPv6 subnet without IPv6":   {subnetConfig("fd00::/64", "", ""), nil, nil, KindInvalid},
		"IPv6 alone":                    {nil, &no, &yes, KindUnsupported},
		"another driver":                {&network.IPAM{Driver: "dhcp"}, nil, nil, KindUnsupported},
		"two IPv4 subnets":              {two("10.1.0.0/24", "10.2.0.0/24"), nil, nil, KindUnsupported},
		"two IPv6 subnets":              {two("fd00:1::/64", "fd00:2::/64"), nil, &yes, KindUnsupported},
		"a configuration, no subnet":    {&network.IPAM{Config: []network.IPAMConfig{{Gateway: netip.MustParseAddr("10.1.0.1")}}}, nil, nil, KindInvalid},
		"IPv6 host bits":                {subnetConfig("fd00::1/64", "", ""), nil, &yes, KindInvalid},
		"no IPv6 host addresses":        {subnetConfig("fd00::/127", "", ""), nil, &yes, KindInvalid},
		"IPv6 loopback":                 {subnetConfig("::1/128", "", ""), nil, &yes, KindInvalid},
		"a link-local subnet":           {subnetConfig("fe80::/64", "", ""), nil, &yes, KindUnsupported},
		"a multicast subnet":            {subnetConfig("ff05::/64", "", ""), nil, &yes, KindInvalid},
		"an IPv4-mapped subnet":         {subnetConfig("::ffff:10.0.0.0/120", "", ""), nil, &yes, KindInvalid},
		"an IPv6 gateway outside":       {subnetConfig("fd00:9::/64", "fd00:8::1", ""), nil, &yes, KindInvalid},
		"an IPv4 range in an IPv6 pool": {subnetConfig("fd00:9::/64", "", "10.0.0.0/24"), nil, &yes, KindInvalid},
	} {
		err := e.checkIPAMLocked(&Network{Name: "x"}, c.ipam, c.ipv4, c.ipv6)
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
	if err := e.checkIPAMLocked(n, ipam, nil, nil); err != nil {
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
	// A stopped container lists its endpoints without addresses.
	for _, s := range e.List(ListOptions{All: true}) {
		if s.ID != "a" {
			continue
		}
		if es := s.NetworkSettings.Networks["front"]; es == nil || es.IPAddress.IsValid() {
			t.Errorf("a stopped container listed %+v on front", es)
		}
	}
}

// The network file names each member's address on a network with a
// subnet.
func TestNetworkFileSubnets(t *testing.T) {
	e := ipamEngine(t)
	add := func(id, address, ip string) {
		nets := map[string]*network.EndpointSettings{"proj_default": {}, "proj_lan": {IPAddress: netip.MustParseAddr(ip), IPPrefixLen: 24, GlobalIPv6Address: netip.MustParseAddr("fd00:5::" + ip[len(ip)-1:]), GlobalIPv6PrefixLen: 64}}
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
	if !strings.Contains(string(b), "\tproj_default proj_lan\t0\t\t\tproj_lan=10.5.0.3/24 proj_lan=fd00:5::3/64\n") || !strings.Contains(string(b), "proj_lan=10.5.0.4/24 proj_lan=fd00:5::4/64\n") {
		t.Errorf("network file:\n%s", b)
	}
}

// A network with IPv6 takes an IPv6 subnet beside its IPv4 one, its own
// or the lowest free /64 of the engine's ULA prefix, and its containers an
// address in it too.
func TestIPv6Networks(t *testing.T) {
	e := ipamEngine(t)
	yes := true
	n := &Network{ID: "n1", Name: "lan", Driver: "bridge"}
	ipam := &network.IPAM{Config: []network.IPAMConfig{{Subnet: netip.MustParsePrefix("10.6.0.0/24")}, {Subnet: netip.MustParsePrefix("fd00:6::/64")}}}
	if err := e.checkIPAMLocked(n, ipam, nil, &yes); err != nil {
		t.Fatal(err)
	}
	if !n.EnableIPv6 || n.IPv6.Subnet != "fd00:6::/64" || n.IPv6.Gateway != "fd00:6::1" || n.IPv6.Auto {
		t.Fatalf("recorded %+v", n)
	}
	if got := n.ipamConfig(); len(got) != 2 || got[0].Subnet.String() != "10.6.0.0/24" || got[1].Gateway.String() != "fd00:6::1" {
		t.Errorf("reported %+v", got)
	}
	e.networks[n.ID] = n
	if err := e.checkIPAMLocked(&Network{Name: "x"}, subnetConfig("fd00:6:0:0:8000::/66", "", ""), nil, &yes); KindOf(err) != KindForbidden {
		t.Errorf("an overlapping IPv6 subnet: %v", err)
	}

	// Without a subnet of its own, a network takes /64s of the ULA prefix
	// in turn, and an IPv4 subnet of Docker's defaults, as its networks
	// with IPv6 are dual-stack.
	ula := e.ulaPrefix()
	if b := ula.Addr().As16(); ula.Bits() != 48 || b[0] != 0xfd {
		t.Fatalf("ULA prefix %s", ula)
	}
	var autos []*Network
	for i, name := range []string{"auto1", "auto2"} {
		a := &Network{ID: name, Name: name, Driver: "bridge"}
		if err := e.checkIPAMLocked(a, nil, nil, &yes); err != nil {
			t.Fatal(err)
		}
		subnet := netip.MustParsePrefix(a.IPv6.Subnet)
		b := ula.Addr().As16()
		b[7] = byte(i)
		v4 := netip.MustParsePrefix(a.Subnet)
		if subnet != netip.PrefixFrom(netip.AddrFrom16(b), 64) || !a.IPv6.Auto || !a.Pool.Auto || !v4.Addr().IsPrivate() {
			t.Errorf("%s took %+v", name, a)
		}
		for _, o := range autos {
			if netip.MustParsePrefix(o.Subnet).Overlaps(v4) {
				t.Errorf("%s took %s, which %s holds", name, v4, o.Name)
			}
		}
		if got := a.ipamConfig(); len(got) != 2 || got[0].Subnet != v4 || got[1].Subnet != subnet {
			t.Errorf("%s reports %+v", name, got)
		}
		e.networks[a.ID] = a
		autos = append(autos, a)
	}

	join := func(n *Network, id, static6 string) (*network.EndpointSettings, error) {
		s := &network.EndpointSettings{}
		if static6 != "" {
			s.IPAMConfig = &network.EndpointIPAMConfig{IPv6Address: netip.MustParseAddr(static6)}
		}
		if err := e.assignNetAddressLocked(n, id, s); err != nil {
			return nil, err
		}
		e.containers[id] = &Container{rec: record{ID: id, Address: "127.88.0.9", Networks: map[string]*network.EndpointSettings{n.Name: s}}}
		return s, nil
	}
	a, err := join(n, "a", "")
	if err != nil || a.IPAddress.String() != "10.6.0.2" || a.GlobalIPv6Address.String() != "fd00:6::2" || a.GlobalIPv6PrefixLen != 64 || a.IPv6Gateway.String() != "fd00:6::1" {
		t.Fatalf("first container: %+v, %v", a, err)
	}
	if b, err := join(n, "b", "fd00:6::50"); err != nil || b.GlobalIPv6Address.String() != "fd00:6::50" || b.IPAddress.String() != "10.6.0.3" {
		t.Fatalf("static IPv6 address: %+v, %v", b, err)
	}
	for static, kind := range map[string]Kind{"fd00:6::50": KindForbidden, "fd00:6::1": KindForbidden, "fd00:7::5": KindInvalid} {
		if _, err := join(n, "d", static); KindOf(err) != kind {
			t.Errorf("static %s: %v, want kind %v", static, err, kind)
		}
	}
	if _, err := join(autos[0], "e", netip.MustParsePrefix(autos[0].IPv6.Subnet).Addr().Next().Next().String()); KindOf(err) != KindInvalid {
		t.Errorf("a static IPv6 address on a network the engine chose the subnet of: %v", err)
	}
	e4 := &network.EndpointSettings{IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParsePrefix(autos[0].Subnet).Addr().Next().Next()}}
	if err := e.assignNetAddressLocked(autos[0], "e4", e4); KindOf(err) != KindInvalid {
		t.Errorf("a static IPv4 address on a network the engine chose the subnet of: %v", err)
	}
	if f, err := join(autos[0], "f", ""); err != nil || f.IPAddress != netip.MustParsePrefix(autos[0].Subnet).Addr().Next().Next() || f.GlobalIPv6Address != netip.MustParsePrefix(autos[0].IPv6.Subnet).Addr().Next().Next() {
		t.Errorf("a container on a network with an engine-chosen subnet: %+v, %v", f, err)
	}

	view := e.networkView(n)
	if !view.EnableIPv6 || len(view.IPAM.Config) != 2 {
		t.Errorf("network view %+v", view.Network)
	}
	if got := firstAddresses("127.88.0.9", e.containers["a"].rec.Networks); len(got) != 2 || got[0] != "10.6.0.2" || got[1] != "fd00:6::2" {
		t.Errorf("hosts file addresses %v", got)
	}
	// A stopped container reports no IPv6 address either.
	c := e.containers["a"]
	if es := c.endpoints()["lan"]; es.GlobalIPv6Address.IsValid() || es.IPv6Gateway.IsValid() {
		t.Errorf("a stopped container reports %+v", es)
	}
}
