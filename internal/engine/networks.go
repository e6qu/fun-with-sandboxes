package engine

import (
	"crypto/sha256"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/egress"
)

// Network records a Docker network. Containers reach each other through their
// stack's ports, so a network carries no traffic of its own; it scopes which
// containers may join it and is reported faithfully to clients.
type Network struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Created    time.Time         `json:"created"`
	Labels     map[string]string `json:"labels"`
	Options    map[string]string `json:"options"`
	Internal   bool              `json:"internal"`
	Attachable bool              `json:"attachable"`
	Predefined bool              `json:"predefined"`
	// Pool is the first IPv4 subnet the network was created with; members
	// on a network without one take their member addresses in
	// memberSubnet.
	Pool
	// EnableIPv6 gives each member an IPv6 address on the network too,
	// from IPv6, its own subnet or a /64 of the engine's ULA prefix.
	EnableIPv6 bool `json:"enableIPv6,omitempty"`
	IPv6       Pool `json:"ipv6,omitzero"`
	// More are the subnets past the first of each family, of either
	// family, in the order the network's IPAM configuration names them.
	More []Pool `json:"more,omitempty"`
	// IPv4Disabled marks an IPv6-only network: it holds no IPv4 subnet,
	// and its members no IPv4 address on it, their member address
	// included.
	IPv4Disabled bool `json:"ipv4Disabled,omitempty"`
}

// pools are the network's subnets of a family, in the order containers
// take addresses from them.
func (n *Network) pools(v4 bool) []Pool {
	var out []Pool
	first := n.IPv6
	if v4 {
		first = n.Pool
	}
	if first.Subnet.IsValid() {
		out = append(out, first)
	}
	for _, p := range n.More {
		if p.is4() == v4 {
			out = append(out, p)
		}
	}
	return out
}

// allPools are the network's subnets of both families.
func (n *Network) allPools() []Pool { return append(n.pools(true), n.pools(false)...) }

// Pool is a subnet of the default IPAM driver, with its gateway, the
// range containers' addresses come from and the addresses reserved for
// other hosts. Auto marks one the engine chose, which takes no address a
// container names, as Docker's do not. A pool holds a subnet or is zero,
// and checkPoolLocked fills one with a gateway inside it; its fields
// encode as the strings an engine before them stored.
type Pool struct {
	Subnet       netip.Prefix          `json:"subnet,omitzero"`
	Gateway      netip.Addr            `json:"gateway,omitzero"`
	IPRange      netip.Prefix          `json:"ipRange,omitzero"`
	AuxAddresses map[string]netip.Addr `json:"auxAddresses,omitempty"`
	Auto         bool                  `json:"auto,omitempty"`
}

// memberSubnet holds the member addresses of networks created without a
// subnet, and memberGateway is their gateway, which no member holds.
var (
	memberSubnet  = netip.MustParsePrefix("127.88.0.0/16")
	memberGateway = netip.MustParseAddr("127.88.0.1")
)

func (p Pool) is4() bool { return p.Subnet.Addr().Is4() }

func (p Pool) config() network.IPAMConfig {
	c := network.IPAMConfig{Subnet: p.Subnet, Gateway: p.Gateway, IPRange: p.IPRange}
	for name, a := range p.AuxAddresses {
		if c.AuxAddress == nil {
			c.AuxAddress = map[string]netip.Addr{}
		}
		c.AuxAddress[name] = a
	}
	return c
}

// ipamConfig is the network's IPAM configuration as Docker reports it:
// its IPv4 subnets, then its IPv6 ones.
func (n *Network) ipamConfig() []network.IPAMConfig {
	out := []network.IPAMConfig{}
	v4 := n.pools(true)
	if len(v4) == 0 && n.Driver == "bridge" && !n.IPv4Disabled {
		out = append(out, network.IPAMConfig{Subnet: memberSubnet, Gateway: memberGateway})
	}
	for _, p := range append(v4, n.pools(false)...) {
		out = append(out, p.config())
	}
	return out
}

// checkIPAMLocked reads a network's IPAM configuration into n: of the
// default driver, any number of IPv4 subnets, none on an IPv6-only
// network, and, with IPv6 enabled, of IPv6 subnets, a /64 of the
// engine's ULA prefix when it names none.
func (e *Engine) checkIPAMLocked(n *Network, ipam *network.IPAM, ipv4, ipv6 *bool) error {
	n.EnableIPv6 = ipv6 != nil && *ipv6
	n.IPv4Disabled = ipv4 != nil && !*ipv4
	if n.IPv4Disabled && !n.EnableIPv6 {
		return invalid("network %s: IPv4 or IPv6 must be enabled", n.Name)
	}
	var v4, v6 []network.IPAMConfig
	if ipam != nil {
		if ipam.Driver != "" && ipam.Driver != "default" {
			return unsupported("IPAM driver %q is not supported; only \"default\" is", ipam.Driver)
		}
		if len(ipam.Options) > 0 {
			return unsupported("IPAM options are not supported: the default IPAM driver takes none")
		}
		for i := range ipam.Config {
			c := &ipam.Config[i]
			if !c.Subnet.IsValid() {
				return invalid("network %s: an IPAM configuration needs a subnet", n.Name)
			}
			if c.Subnet.Addr().Is4() {
				v4 = append(v4, *c)
			} else {
				v6 = append(v6, *c)
			}
		}
	}
	if n.IPv4Disabled && len(v4) > 0 {
		return invalid("network %s: an IPv4 subnet needs IPv4 enabled on the network", n.Name)
	}
	if !n.EnableIPv6 && len(v6) > 0 {
		return invalid("network %s: an IPv6 subnet needs IPv6 enabled on the network", n.Name)
	}
	var pools []Pool
	add := func(c network.IPAMConfig, auto bool) error {
		p, err := e.checkPoolLocked(c, pools)
		if err != nil {
			return err
		}
		p.Auto = auto
		pools = append(pools, p)
		return nil
	}
	if len(v4) == 0 && n.EnableIPv6 && !n.IPv4Disabled {
		// A network with IPv6 is dual-stack unless IPv4 is disabled, as
		// Docker's are: it takes an IPv4 subnet of the defaults too, so
		// its members hold an address of each family on it, as kind's
		// nodes need on its network.
		subnet, err := e.autoSubnetLocked()
		if err != nil {
			return err
		}
		if err := add(network.IPAMConfig{Subnet: subnet}, true); err != nil {
			return err
		}
	}
	for _, c := range v4 {
		if err := add(c, false); err != nil {
			return err
		}
	}
	if n.EnableIPv6 && len(v6) == 0 {
		subnet, err := e.ulaSubnetLocked()
		if err != nil {
			return err
		}
		if err := add(network.IPAMConfig{Subnet: subnet}, true); err != nil {
			return err
		}
	}
	for _, c := range v6 {
		if err := add(c, false); err != nil {
			return err
		}
	}
	n.Pool, n.IPv6, n.More = Pool{}, Pool{}, nil
	for _, p := range pools {
		switch {
		case p.is4() && !n.Subnet.IsValid():
			n.Pool = p
		case !p.is4() && !n.IPv6.Subnet.IsValid():
			n.IPv6 = p
		default:
			n.More = append(n.More, p)
		}
	}
	return nil
}

// reservedBlocks hold addresses no network's subnet may take: the host's
// own, an interface's link-local ones, multicast groups and IPv4-mapped
// IPv6 addresses.
var reservedBlocks = []struct {
	block netip.Prefix
	kind  Kind
	why   string
}{
	{netip.MustParsePrefix("127.0.0.0/8"), KindUnsupported, "the loopback block 127.0.0.0/8, which holds the host's own addresses"},
	{netip.MustParsePrefix("::1/128"), KindUnsupported, "the loopback address ::1, which is the host's own"},
	{netip.MustParsePrefix("fe80::/10"), KindUnsupported, "the link-local block fe80::/10, which holds each host interface's own addresses"},
	{netip.MustParsePrefix("ff00::/8"), KindInvalid, "the multicast block ff00::/8, which names groups, not hosts"},
	{netip.MustParsePrefix("::ffff:0:0/96"), KindInvalid, "the IPv4-mapped block ::ffff:0:0/96, which names IPv4 addresses"},
}

// checkPoolLocked reads a subnet's configuration into a pool: its
// gateway, by default its first host address, its range and its
// auxiliary addresses, inside it, and the subnet outside the reserved
// blocks, every network's subnets and the pools its own network has
// taken so far, siblings.
func (e *Engine) checkPoolLocked(c network.IPAMConfig, siblings []Pool) (Pool, error) {
	subnet := c.Subnet
	if subnet.Masked() != subnet {
		return Pool{}, invalid("invalid subnet %s : it should be %s", subnet, subnet.Masked())
	}
	most := 126
	if subnet.Addr().Is4() {
		most = 30
	}
	if subnet.Bits() > most {
		return Pool{}, invalid("subnet %s holds no addresses for containers", subnet)
	}
	for _, r := range reservedBlocks {
		if subnet.Overlaps(r.block) {
			return Pool{}, errorf(r.kind, "subnet %s overlaps %s", subnet, r.why)
		}
	}
	taken := siblings
	for _, o := range e.networks {
		taken = append(taken, o.allPools()...)
	}
	for _, p := range taken {
		if p.Subnet.Overlaps(subnet) {
			return Pool{}, forbidden("invalid pool request: Pool overlaps with other one on this address space")
		}
	}
	p := Pool{Subnet: subnet, Gateway: subnet.Addr().Next()}
	if c.Gateway.IsValid() {
		if !subnet.Contains(c.Gateway) {
			return Pool{}, invalid("no matching subnet for gateway %s", c.Gateway)
		}
		p.Gateway = c.Gateway
	}
	if r := c.IPRange; r.IsValid() {
		if r.Masked() != r || r.Addr().Is4() != subnet.Addr().Is4() || r.Bits() < subnet.Bits() || !subnet.Contains(r.Addr()) {
			return Pool{}, invalid("no matching subnet for range %s", r)
		}
		p.IPRange = r
	}
	for name, a := range c.AuxAddress {
		if !subnet.Contains(a) {
			return Pool{}, invalid("no matching subnet for aux-address %s", a)
		}
		if p.AuxAddresses == nil {
			p.AuxAddresses = map[string]netip.Addr{}
		}
		p.AuxAddresses[name] = a
	}
	return p, nil
}

// ulaPrefix is the /48 networks with IPv6 and no subnet of their own take
// their /64s from, as dockerd derives its: fd and 40 bits of a hash of
// the engine's identity, here its state directory.
func (e *Engine) ulaPrefix() netip.Prefix {
	h := sha256.Sum256([]byte(e.cfg.Root))
	var b [16]byte
	b[0] = 0xfd
	copy(b[1:6], h[:5])
	return netip.PrefixFrom(netip.AddrFrom16(b), 48)
}

// autoSubnetLocked is the first of Docker's default IPv4 pools, past
// docker0's 172.17.0.0/16, that no network holds and no interface of the
// host's addresses lies in: 172.18.0.0/16 to 172.31.0.0/16, then the /20s
// of 192.168.0.0/16.
func (e *Engine) autoSubnetLocked() (netip.Prefix, error) {
	var candidates []netip.Prefix
	for b := 18; b <= 31; b++ {
		candidates = append(candidates, netip.PrefixFrom(netip.AddrFrom4([4]byte{172, byte(b), 0, 0}), 16))
	}
	for b := 0; b < 256; b += 16 {
		candidates = append(candidates, netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(b), 0}), 20))
	}
	var host []netip.Prefix
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if p, err := netip.ParsePrefix(ipn.String()); err == nil && p.Addr().Is4() {
					host = append(host, p.Masked())
				}
			}
		}
	}
next:
	for _, c := range candidates {
		for _, o := range e.networks {
			for _, p := range o.pools(true) {
				if p.Subnet.Overlaps(c) {
					continue next
				}
			}
		}
		for _, h := range host {
			if h.Overlaps(c) {
				continue next
			}
		}
		return c, nil
	}
	return netip.Prefix{}, forbidden("could not find an available, non-overlapping IPv4 address pool among the defaults to assign to the network")
}

// ulaSubnetLocked is the lowest /64 of the engine's ULA prefix no
// network's subnet overlaps.
func (e *Engine) ulaSubnetLocked() (netip.Prefix, error) {
	base := e.ulaPrefix().Addr().As16()
	for id := 0; id < 1<<16; id++ {
		b := base
		b[6], b[7] = byte(id>>8), byte(id)
		subnet := netip.PrefixFrom(netip.AddrFrom16(b), 64)
		free := true
		for _, o := range e.networks {
			for _, p := range o.pools(false) {
				if p.Subnet.Overlaps(subnet) {
					free = false
				}
			}
		}
		if free {
			return subnet, nil
		}
	}
	return netip.Prefix{}, forbidden("could not find an available, non-overlapping IPv6 address pool among the defaults to assign to the network")
}

// assignNetAddressLocked gives the endpoint s of container id its
// addresses on n: of each family the network has subnets of, the one its
// IPAM configuration names, or the lowest free one of the first subnet
// with room; on one with no IPv4 subnet, no IPv4 address, as the
// container's member address serves every such network but an
// IPv6-only one.
func (e *Engine) assignNetAddressLocked(n *Network, id string, s *network.EndpointSettings) error {
	var static4, static6 netip.Addr
	if s.IPAMConfig != nil {
		static4, static6 = s.IPAMConfig.IPv4Address, s.IPAMConfig.IPv6Address
	}
	s.IPAddress, s.IPPrefixLen, s.Gateway, s.MacAddress = netip.Addr{}, 0, netip.Addr{}, nil
	s.GlobalIPv6Address, s.GlobalIPv6PrefixLen, s.IPv6Gateway = netip.Addr{}, 0, netip.Addr{}
	if static4.IsValid() && n.IPv4Disabled {
		return invalid("network %s is IPv6-only: a container takes no IPv4 address on it", n.Name)
	}
	if (static4.IsValid() && (!n.Subnet.IsValid() || n.Pool.Auto)) || (static6.IsValid() && (!n.IPv6.Subnet.IsValid() || n.IPv6.Auto)) {
		return invalid("user specified IP address is supported only when connecting to networks with user configured subnets")
	}
	if pools := n.pools(true); len(pools) > 0 {
		ip, bits, gw, err := e.pickAddressLocked(n, pools, id, static4, func(es *network.EndpointSettings) netip.Addr { return es.IPAddress })
		if err != nil {
			return err
		}
		s.IPAddress, s.IPPrefixLen, s.Gateway, s.MacAddress = ip, bits, gw, macOf(ip)
	}
	if pools := n.pools(false); len(pools) > 0 {
		ip, bits, gw, err := e.pickAddressLocked(n, pools, id, static6, func(es *network.EndpointSettings) netip.Addr { return es.GlobalIPv6Address })
		if err != nil {
			return err
		}
		s.GlobalIPv6Address, s.GlobalIPv6PrefixLen, s.IPv6Gateway = ip, bits, gw
		if len(s.MacAddress) == 0 {
			s.MacAddress = macOf(ip)
		}
	}
	return nil
}

// pickAddressLocked is container id's address in the pools, all of one
// family, of network n, with its subnet's length and gateway: static,
// when valid, in the pool whose subnet holds it, or the lowest of the
// first pool's range that no other container's endpoint, as addr reads
// it, no gateway and no reserved host holds, else of the next pool's.
func (e *Engine) pickAddressLocked(n *Network, pools []Pool, id string, static netip.Addr, addr func(*network.EndpointSettings) netip.Addr) (netip.Addr, int, netip.Addr, error) {
	v4 := pools[0].is4()
	taken := map[netip.Addr]bool{}
	for _, p := range pools {
		taken[p.Gateway], taken[p.Subnet.Addr()] = true, true
		if v4 {
			taken[broadcast(p.Subnet)] = true
		}
		for _, a := range p.AuxAddresses {
			taken[a] = true
		}
	}
	for _, o := range e.containers {
		if es := o.rec.Networks[n.Name]; o.rec.ID != id && es != nil && addr(es).IsValid() {
			taken[addr(es)] = true
		}
	}
	if static.IsValid() {
		for _, p := range pools {
			if static.Is4() == v4 && p.Subnet.Contains(static) {
				if taken[static] {
					return netip.Addr{}, 0, netip.Addr{}, forbidden("Address already in use")
				}
				return static, p.Subnet.Bits(), p.Gateway, nil
			}
		}
		return netip.Addr{}, 0, netip.Addr{}, invalid("no configured subnet or ip-range contain the IP address %s", static)
	}
	for _, p := range pools {
		pool := p.Subnet
		if p.IPRange.IsValid() {
			pool = p.IPRange
		}
		for a := pool.Addr(); pool.Contains(a); a = a.Next() {
			if !taken[a] {
				return a, p.Subnet.Bits(), p.Gateway, nil
			}
		}
	}
	family := "IPv6"
	if v4 {
		family = "IPv4"
	}
	return netip.Addr{}, 0, netip.Addr{}, forbidden("no available %s addresses on this network's address pools: %s (%s)", family, n.Name, n.ID)
}

func broadcast(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | (uint32(1)<<(32-p.Bits()) - 1)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// macOf is the MAC address Docker gives an endpoint at address a: 02:42
// and the address's four bytes, of an IPv6 address its last four.
func macOf(a netip.Addr) network.HardwareAddr {
	b := a.As16()
	return network.HardwareAddr{0x02, 0x42, b[12], b[13], b[14], b[15]}
}

func (e *Engine) networksPath() string { return filepath.Join(e.cfg.Root, "networks.json") }

func (e *Engine) loadNetworks() error {
	if _, err := readJSON(e.networksPath(), &e.networks); err != nil {
		return err
	}
	if e.networks == nil {
		e.networks = map[string]*Network{}
	}
	for _, n := range []struct{ name, driver string }{{"bridge", "bridge"}, {"host", "host"}, {"none", "null"}} {
		if _, err := e.networkLocked(n.name); err != nil {
			id := newID()
			e.networks[id] = &Network{ID: id, Name: n.name, Driver: n.driver, Created: time.Now().UTC(), Predefined: true, Labels: map[string]string{}, Options: map[string]string{}}
		}
	}
	// The default bridge network holds docker0's subnet, as Docker's does:
	// its containers hold an address of it beside their member address,
	// which Kubernetes takes for a node's, as it takes no loopback one. An
	// engine whose state predates it gives it the subnet unless a network
	// of its own took it.
	if b, err := e.networkLocked("bridge"); err == nil && !b.Subnet.IsValid() {
		if p, err := e.checkPoolLocked(network.IPAMConfig{Subnet: bridgeSubnet}, nil); err == nil {
			p.Auto = true
			b.Pool = p
		}
	}
	return e.saveNetworksLocked()
}

// bridgeSubnet is the default bridge network's, docker0's.
var bridgeSubnet = netip.MustParsePrefix("172.17.0.0/16")

func (e *Engine) saveNetworksLocked() error { return writeJSON(e.networksPath(), e.networks) }

func (e *Engine) networkLocked(ref string) (*Network, error) {
	if n, ok := e.networks[ref]; ok {
		return n, nil
	}
	for _, n := range e.networks {
		if n.Name == ref {
			return n, nil
		}
	}
	var match *Network
	for id, n := range e.networks {
		if len(ref) > 0 && strings.HasPrefix(id, ref) {
			if match != nil {
				return nil, invalid("network %s is ambiguous", ref)
			}
			match = n
		}
	}
	if match == nil {
		return nil, notFound("network %s not found", ref)
	}
	return match, nil
}

type NetworkCreate struct {
	Name       string
	Driver     string
	Internal   bool
	Attachable bool
	EnableIPv4 *bool
	EnableIPv6 *bool
	IPAM       *network.IPAM
	Options    map[string]string
	Labels     map[string]string
}

func (e *Engine) NetworkCreate(req NetworkCreate) (network.CreateResponse, error) {
	if req.Name == "" {
		return network.CreateResponse{}, invalid("network name is required")
	}
	if req.Driver == "" {
		req.Driver = "bridge"
	}
	if req.Driver != "bridge" {
		return network.CreateResponse{}, unsupported("network driver %q is not supported; only \"bridge\" is", req.Driver)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.networkLocked(req.Name); err == nil {
		return network.CreateResponse{}, conflict("network with name %s already exists", req.Name)
	}
	labels := req.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	opts := req.Options
	if opts == nil {
		opts = map[string]string{}
	}
	if err := checkNetworkOptions(opts); err != nil {
		return network.CreateResponse{}, err
	}
	n := &Network{ID: newID(), Name: req.Name, Driver: req.Driver, Created: time.Now().UTC(), Labels: labels, Options: opts, Internal: req.Internal, Attachable: req.Attachable}
	if err := e.checkIPAMLocked(n, req.IPAM, req.EnableIPv4, req.EnableIPv6); err != nil {
		return network.CreateResponse{}, err
	}
	e.networks[n.ID] = n
	if err := e.saveNetworksLocked(); err != nil {
		delete(e.networks, n.ID)
		return network.CreateResponse{}, err
	}
	e.events.publish(events.NetworkEventType, events.ActionCreate, n.ID, map[string]string{"name": n.Name, "type": n.Driver})
	return network.CreateResponse{ID: n.ID}, nil
}

func (e *Engine) NetworkInspect(ref string) (network.Inspect, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, err := e.networkLocked(ref)
	if err != nil {
		return network.Inspect{}, err
	}
	return e.networkView(n), nil
}

func (e *Engine) networkView(n *Network) network.Inspect {
	containers := map[string]network.EndpointResource{}
	for _, c := range e.containers {
		if _, ok := c.rec.Networks[n.Name]; ok && c.isRunning() {
			es := c.endpoint(n.Name)
			r := network.EndpointResource{Name: c.rec.Name, EndpointID: es.EndpointID, MacAddress: es.MacAddress}
			if es.IPAddress.IsValid() {
				r.IPv4Address = netip.PrefixFrom(es.IPAddress, es.IPPrefixLen)
			}
			if es.GlobalIPv6Address.IsValid() {
				r.IPv6Address = netip.PrefixFrom(es.GlobalIPv6Address, es.GlobalIPv6PrefixLen)
			}
			containers[c.rec.ID] = r
		}
	}
	return network.Inspect{
		Network: network.Network{
			Name: n.Name, ID: n.ID, Created: n.Created, Scope: "local", Driver: n.Driver,
			EnableIPv4: n.Driver == "bridge" && !n.IPv4Disabled, EnableIPv6: n.EnableIPv6, IPAM: network.IPAM{Driver: "default", Config: n.ipamConfig()},
			Internal: n.Internal, Attachable: n.Attachable, Options: n.Options, Labels: n.Labels,
		},
		Containers: containers,
	}
}

func (e *Engine) NetworkList(f Filters) []network.Summary {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []network.Summary
	for _, n := range e.networks {
		if !f.matchName("name", n.Name) || !f.matchExact("id", n.ID) || !f.matchExact("driver", n.Driver) || !f.matchLabels(n.Labels) {
			continue
		}
		if f.has("type") {
			typ := "custom"
			if n.Predefined {
				typ = "builtin"
			}
			if !f.matchExact("type", typ) {
				continue
			}
		}
		out = append(out, network.Summary{Network: e.networkView(n).Network})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (e *Engine) NetworkRemove(ref string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, err := e.networkLocked(ref)
	if err != nil {
		return err
	}
	if n.Predefined {
		return errorf(KindConflict, "%s is a pre-defined network and cannot be removed", n.Name)
	}
	for _, c := range e.containers {
		if _, ok := c.rec.Networks[n.Name]; ok {
			return conflict("error while removing network: network %s has active endpoints (%s)", n.Name, c.rec.Name)
		}
	}
	delete(e.networks, n.ID)
	e.events.publish(events.NetworkEventType, events.ActionDestroy, n.ID, map[string]string{"name": n.Name, "type": n.Driver})
	return e.saveNetworksLocked()
}

// checkNetworkLocked refuses networks a container may not join. A network
// another stack's containers are on joins the stacks: their members reach
// one another on it through their routers.
func (e *Engine) checkNetworkLocked(name string) (*Network, error) {
	n, err := e.networkLocked(name)
	if err != nil {
		return nil, err
	}
	if n.Name == "host" {
		return nil, unsupported("network mode \"host\" is not supported: every container is confined to its own stack's ports")
	}
	return n, nil
}

// NetworkConnect adds the container to the network: its members, of any
// stack, reach the container by name, and it reaches them.
func (e *Engine) NetworkConnect(ref string, req network.ConnectRequest) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, err := e.networkLocked(ref)
	if err != nil {
		return err
	}
	c, err := e.containerLocked(req.Container)
	if err != nil {
		return err
	}
	if !onNetwork(c) && c.rec.HostConfig.NetworkMode != "none" {
		return forbidden("container sharing network namespace with another container or host cannot be connected to any other network")
	}
	if _, err := e.checkNetworkLocked(n.Name); err != nil {
		return err
	}
	if err := checkMAC(req.EndpointConfig); err != nil {
		return err
	}
	// The container's lock is released before the network files are
	// written: a nested engine's publishLocked reads every member's state
	// under its lock, c's among them.
	c.mu.Lock()
	if _, ok := c.rec.Networks[n.Name]; ok {
		c.mu.Unlock()
		return conflict("endpoint with name %s already exists in network %s", c.rec.Name, n.Name)
	}
	s := &network.EndpointSettings{}
	if req.EndpointConfig != nil {
		s = req.EndpointConfig.Copy()
	}
	s.NetworkID, s.EndpointID = n.ID, newID()
	s.DNSNames = uniqStrings(append(append([]string{c.rec.Name}, s.Aliases...), c.rec.Service))
	if err := e.assignNetAddressLocked(n, c.rec.ID, s); err != nil {
		c.mu.Unlock()
		return err
	}
	c.rec.Networks[n.Name] = s
	err = c.save()
	c.mu.Unlock()
	e.events.publish(events.NetworkEventType, events.ActionConnect, n.ID, map[string]string{"container": c.rec.ID, "name": n.Name, "type": n.Driver})
	if err != nil {
		return err
	}
	return e.writeNetworkLocked(c.rec.Stack)
}

func (e *Engine) NetworkDisconnect(ref, containerRef string, force bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, err := e.networkLocked(ref)
	if err != nil {
		return err
	}
	c, err := e.containerLocked(containerRef)
	if err != nil {
		if force {
			return nil
		}
		return err
	}
	// As NetworkConnect, unlocked before the network files are written.
	c.mu.Lock()
	if _, ok := c.rec.Networks[n.Name]; !ok {
		c.mu.Unlock()
		return invalid("container %s is not connected to network %s", c.rec.Name, n.Name)
	}
	delete(c.rec.Networks, n.Name)
	err = c.save()
	c.mu.Unlock()
	e.events.publish(events.NetworkEventType, events.ActionDisconnect, n.ID, map[string]string{"container": c.rec.ID, "name": n.Name, "type": n.Driver})
	if err != nil {
		return err
	}
	return e.writeNetworkLocked(c.rec.Stack)
}

// NetworkPrune removes custom networks no container uses.
func (e *Engine) NetworkPrune(f Filters) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var removed []string
	for id, n := range e.networks {
		if n.Predefined || !f.matchLabels(n.Labels) {
			continue
		}
		used := false
		for _, c := range e.containers {
			if _, ok := c.rec.Networks[n.Name]; ok {
				used = true
			}
		}
		if used {
			continue
		}
		delete(e.networks, id)
		removed = append(removed, n.Name)
		e.events.publish(events.NetworkEventType, events.ActionDestroy, n.ID, map[string]string{"name": n.Name, "type": n.Driver})
	}
	return removed, e.saveNetworksLocked()
}

// The bridge driver's options the engine takes.
const (
	optMasquerade  = "com.docker.network.bridge.enable_ip_masquerade"
	optICC         = "com.docker.network.bridge.enable_icc"
	optHostBinding = "com.docker.network.bridge.host_binding_ipv4"
	optBridgeName  = "com.docker.network.bridge.name"
	optMTU         = "com.docker.network.driver.mtu"
)

// checkNetworkOptions refuses a driver option the engine cannot honour.
// It honours enable_ip_masquerade, false for a network that gives no
// internet access; enable_icc, false for one whose members do not reach
// one another over it; host_binding_ipv4, the host address its members'
// ports publish on by default; the bridge's name, which inspect reports,
// as no host bridge exists; an MTU, which a network on loopback has no
// use for; and fws.egress.allow.
func checkNetworkOptions(opts map[string]string) error {
	for k, v := range opts {
		switch k {
		case egress.AllowKey:
			if _, err := egress.ParseAllow(v); err != nil {
				return invalid("option %s: %v", k, err)
			}
		case optMasquerade, optICC:
			if v != "true" && v != "false" {
				return invalid("option %s: %q is not true or false", k, v)
			}
		case optHostBinding:
			if a, err := netip.ParseAddr(v); err != nil || !a.Is4() {
				return invalid("option %s: %q is not an IPv4 address", k, v)
			}
		case optBridgeName:
		case optMTU:
			if n, err := strconv.Atoi(v); err != nil || n < 68 || n > 65535 {
				return invalid("option %s: %q is not an MTU", k, v)
			}
		default:
			return unsupported("network option %s is not supported", k)
		}
	}
	return nil
}

// masquerades reports whether the network gives its members internet
// access, as a bridge with IP masquerading does.
func (n *Network) masquerades() bool { return !n.Internal && n.Options[optMasquerade] != "false" }

// connects reports whether the network's members reach one another over
// it, as a bridge with inter-container communication does.
func (n *Network) connects() bool { return n.Options[optICC] != "false" }
