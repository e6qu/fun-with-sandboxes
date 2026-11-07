package engine

import (
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
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
	// Subnet, with its gateway, the range containers' addresses come
	// from and the addresses reserved for other hosts, is the IPv4
	// subnet the network was created with; members on a network without
	// one take their member addresses in memberSubnet.
	Subnet       string            `json:"subnet,omitempty"`
	Gateway      string            `json:"gateway,omitempty"`
	IPRange      string            `json:"ipRange,omitempty"`
	AuxAddresses map[string]string `json:"auxAddresses,omitempty"`
}

// memberSubnet holds the member addresses of networks created without a
// subnet, and memberGateway is their gateway, which no member holds.
const (
	memberSubnet  = "127.88.0.0/16"
	memberGateway = "127.88.0.1"
)

// ipamConfig is the network's IPAM configuration as Docker reports it.
func (n *Network) ipamConfig() []network.IPAMConfig {
	switch {
	case n.Subnet != "":
		c := network.IPAMConfig{Subnet: netip.MustParsePrefix(n.Subnet), Gateway: netip.MustParseAddr(n.Gateway)}
		if n.IPRange != "" {
			c.IPRange = netip.MustParsePrefix(n.IPRange)
		}
		for name, a := range n.AuxAddresses {
			if c.AuxAddress == nil {
				c.AuxAddress = map[string]netip.Addr{}
			}
			c.AuxAddress[name] = netip.MustParseAddr(a)
		}
		return []network.IPAMConfig{c}
	case n.Driver == "bridge":
		return []network.IPAMConfig{{Subnet: netip.MustParsePrefix(memberSubnet), Gateway: netip.MustParseAddr(memberGateway)}}
	}
	return []network.IPAMConfig{}
}

// checkIPAMLocked reads a network's IPAM configuration into n: one IPv4
// subnet of the default driver, as Docker's default IPAM driver holds it,
// outside the loopback block and every other network's subnet.
func (e *Engine) checkIPAMLocked(n *Network, ipam *network.IPAM, ipv6 *bool) error {
	if ipv6 != nil && *ipv6 {
		return unsupported("network %s: IPv6 is not supported: the container library presents IPv4 member addresses alone", n.Name)
	}
	if ipam == nil {
		return nil
	}
	if ipam.Driver != "" && ipam.Driver != "default" {
		return unsupported("IPAM driver %q is not supported; only \"default\" is", ipam.Driver)
	}
	if len(ipam.Options) > 0 {
		return unsupported("IPAM options are not supported: the default IPAM driver takes none")
	}
	if len(ipam.Config) == 0 {
		return nil
	}
	if len(ipam.Config) > 1 {
		return unsupported("network %s: more than one subnet is not supported: a container holds one address on a network", n.Name)
	}
	c := ipam.Config[0]
	subnet := c.Subnet
	if !subnet.IsValid() {
		return invalid("network %s: an IPAM configuration needs a subnet", n.Name)
	}
	if !subnet.Addr().Is4() {
		return unsupported("network %s: IPv6 subnets are not supported: the container library presents IPv4 member addresses alone", n.Name)
	}
	if subnet.Masked() != subnet {
		return invalid("invalid subnet %s : it should be %s", subnet, subnet.Masked())
	}
	if subnet.Bits() > 30 {
		return invalid("subnet %s holds no addresses for containers", subnet)
	}
	if subnet.Overlaps(netip.MustParsePrefix("127.0.0.0/8")) {
		return unsupported("subnet %s overlaps the loopback block 127.0.0.0/8, which holds the host's own addresses", subnet)
	}
	for _, o := range e.networks {
		if o.Subnet != "" && netip.MustParsePrefix(o.Subnet).Overlaps(subnet) {
			return forbidden("invalid pool request: Pool overlaps with other one on this address space")
		}
	}
	gateway := subnet.Addr().Next()
	if c.Gateway.IsValid() {
		if !subnet.Contains(c.Gateway) {
			return invalid("no matching subnet for gateway %s", c.Gateway)
		}
		gateway = c.Gateway
	}
	if r := c.IPRange; r.IsValid() {
		if r.Masked() != r || !r.Addr().Is4() || r.Bits() < subnet.Bits() || !subnet.Contains(r.Addr()) {
			return invalid("no matching subnet for range %s", r)
		}
		n.IPRange = r.String()
	}
	for name, a := range c.AuxAddress {
		if !subnet.Contains(a) {
			return invalid("no matching subnet for aux-address %s", a)
		}
		if n.AuxAddresses == nil {
			n.AuxAddresses = map[string]string{}
		}
		n.AuxAddresses[name] = a.String()
	}
	n.Subnet, n.Gateway = subnet.String(), gateway.String()
	return nil
}

// assignNetAddressLocked gives the endpoint s of container id its address
// on n: on a network with a subnet, the one its IPAM configuration names,
// or the lowest of the range no container, gateway or reserved host
// holds; on any other, none, as the container's member address serves
// every such network.
func (e *Engine) assignNetAddressLocked(n *Network, id string, s *network.EndpointSettings) error {
	var static netip.Addr
	if s.IPAMConfig != nil {
		if s.IPAMConfig.IPv6Address.IsValid() {
			return unsupported("IPv6 addresses are not supported: the container library presents IPv4 member addresses alone")
		}
		static = s.IPAMConfig.IPv4Address
	}
	s.IPAddress, s.IPPrefixLen, s.Gateway, s.MacAddress = netip.Addr{}, 0, netip.Addr{}, nil
	if n.Subnet == "" {
		if static.IsValid() {
			return invalid("user specified IP address is supported only when connecting to networks with user configured subnets")
		}
		return nil
	}
	subnet := netip.MustParsePrefix(n.Subnet)
	taken := map[netip.Addr]bool{netip.MustParseAddr(n.Gateway): true, subnet.Addr(): true, broadcast(subnet): true}
	for _, a := range n.AuxAddresses {
		taken[netip.MustParseAddr(a)] = true
	}
	for _, o := range e.containers {
		if es := o.rec.Networks[n.Name]; o.rec.ID != id && es != nil && es.IPAddress.IsValid() {
			taken[es.IPAddress] = true
		}
	}
	ip := static
	if ip.IsValid() {
		if !ip.Is4() || !subnet.Contains(ip) {
			return invalid("no configured subnet or ip-range contain the IP address %s", ip)
		}
		if taken[ip] {
			return forbidden("Address already in use")
		}
	} else {
		pool := subnet
		if n.IPRange != "" {
			pool = netip.MustParsePrefix(n.IPRange)
		}
		for a := pool.Addr(); pool.Contains(a); a = a.Next() {
			if !taken[a] {
				ip = a
				break
			}
		}
		if !ip.IsValid() {
			return forbidden("no available IPv4 addresses on this network's address pools: %s (%s)", n.Name, n.ID)
		}
	}
	s.IPAddress, s.IPPrefixLen, s.Gateway, s.MacAddress = ip, subnet.Bits(), netip.MustParseAddr(n.Gateway), macOf(ip)
	return nil
}

func broadcast(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | (uint32(1)<<(32-p.Bits()) - 1)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// macOf is the MAC address Docker gives an endpoint at address a: 02:42
// and the address's four bytes.
func macOf(a netip.Addr) network.HardwareAddr {
	b := a.As4()
	return network.HardwareAddr{0x02, 0x42, b[0], b[1], b[2], b[3]}
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
	return e.saveNetworksLocked()
}

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
	n := &Network{ID: newID(), Name: req.Name, Driver: req.Driver, Created: time.Now().UTC(), Labels: labels, Options: opts, Internal: req.Internal, Attachable: req.Attachable}
	if err := e.checkIPAMLocked(n, req.IPAM, req.EnableIPv6); err != nil {
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
			containers[c.rec.ID] = r
		}
	}
	return network.Inspect{
		Network: network.Network{
			Name: n.Name, ID: n.ID, Created: n.Created, Scope: "local", Driver: n.Driver,
			EnableIPv4: n.Driver == "bridge", IPAM: network.IPAM{Driver: "default", Config: n.ipamConfig()},
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
	if strings.HasPrefix(string(c.rec.HostConfig.NetworkMode), "container:") {
		return forbidden("container sharing network namespace with another container or host cannot be connected to any other network")
	}
	if _, err := e.checkNetworkLocked(n.Name); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.rec.Networks[n.Name]; ok {
		return conflict("endpoint with name %s already exists in network %s", c.rec.Name, n.Name)
	}
	s := &network.EndpointSettings{}
	if req.EndpointConfig != nil {
		s = req.EndpointConfig.Copy()
	}
	s.NetworkID, s.EndpointID = n.ID, newID()
	s.DNSNames = uniqStrings(append(append([]string{c.rec.Name}, s.Aliases...), c.rec.Service))
	if err := e.assignNetAddressLocked(n, c.rec.ID, s); err != nil {
		return err
	}
	c.rec.Networks[n.Name] = s
	e.events.publish(events.NetworkEventType, events.ActionConnect, n.ID, map[string]string{"container": c.rec.ID, "name": n.Name, "type": n.Driver})
	if err := c.save(); err != nil {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.rec.Networks[n.Name]; !ok {
		return invalid("container %s is not connected to network %s", c.rec.Name, n.Name)
	}
	delete(c.rec.Networks, n.Name)
	e.events.publish(events.NetworkEventType, events.ActionDisconnect, n.ID, map[string]string{"container": c.rec.ID, "name": n.Name, "type": n.Driver})
	if err := c.save(); err != nil {
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
