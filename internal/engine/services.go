package engine

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/alloc"
	stackmembers "github.com/e6qu/fun-with-sandboxes/internal/members"
	"github.com/e6qu/fun-with-sandboxes/internal/portproxy"
	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// Service is a virtual address on a network, as a Kubernetes Service's
// cluster IP is: its members reach it by address and by name, and the
// engine carries each connection to one of its endpoints, as kube-proxy's
// userspace mode does.
type Service struct {
	// Name and Namespace give the service's names: name.namespace,
	// name.namespace.svc and name.namespace.svc.<domain>, and name alone.
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// ClusterIP is the service's address, IPv4 or IPv6, which its names
	// resolve to.
	ClusterIP netip.Addr `json:"clusterIP"`
	// ClusterIPs are a dual-stack service's addresses: its ClusterIP and
	// one of the other family, which its names resolve to as well.
	ClusterIPs []netip.Addr `json:"clusterIPs,omitempty"`
	// ExternalIPs are addresses the service answers at as at its cluster
	// IPs, each of a family one of them has; its names do not resolve to
	// them.
	ExternalIPs []netip.Addr `json:"externalIPs,omitempty"`
	// SessionAffinity ClientIP keeps each client on the endpoint it
	// reached while it comes back within AffinitySeconds of its last
	// connection or UDP flow; empty or None takes the endpoints by turns.
	SessionAffinity SessionAffinity `json:"sessionAffinity,omitempty"`
	// AffinitySeconds is ClientIP affinity's timeout; 0 is
	// Kubernetes' default, three hours.
	AffinitySeconds int `json:"sessionAffinityTimeout,omitempty"`
	// ExternalName, for a service without addresses or ports, is the name
	// its names stand for, as a CNAME record's do.
	ExternalName string        `json:"externalName,omitempty"`
	Ports        []ServicePort `json:"ports"`
}

// ServicePort is a TCP or UDP port of a service and the endpoints behind it: the
// addresses of members on the service's network, and their ports.
type ServicePort struct {
	Port     int             `json:"port"`
	Protocol ServiceProtocol `json:"protocol"`
	// NodePort, when set, is the port of the engine's node where the
	// service port answers too, as a NodePort Service's does: for an
	// engine nested in a privileged container, that container's port,
	// which the members of its networks reach.
	NodePort  int              `json:"nodePort,omitempty"`
	Endpoints []netip.AddrPort `json:"endpoints"`
}

// SessionAffinity is a service's session affinity, as Kubernetes spells
// it; checkService takes either in any case, and empty for None.
type SessionAffinity string

const (
	AffinityNone     SessionAffinity = "None"
	AffinityClientIP SessionAffinity = "ClientIP"
)

func (a SessionAffinity) clientIP() bool {
	return strings.EqualFold(string(a), string(AffinityClientIP))
}

func (a SessionAffinity) valid() bool {
	return a == "" || a.clientIP() || strings.EqualFold(string(a), string(AffinityNone))
}

// ServiceProtocol is a service port's protocol, as Kubernetes spells it;
// checkService takes either in any case, and empty for TCP.
type ServiceProtocol string

const (
	ProtocolTCP ServiceProtocol = "TCP"
	ProtocolUDP ServiceProtocol = "UDP"
)

func (p ServiceProtocol) udp() bool { return strings.EqualFold(string(p), string(ProtocolUDP)) }

func (p ServiceProtocol) valid() bool {
	return p == "" || p.udp() || strings.EqualFold(string(p), string(ProtocolTCP))
}

// ClusterDomain is the domain of the services' names.
const ClusterDomain = "cluster.local"

// defaultAffinity is how long ClientIP affinity keeps a client's endpoint
// when the service names no timeout, as Kubernetes defaults it; maxAffinity
// is the longest it allows.
const (
	defaultAffinity = 3 * time.Hour
	maxAffinity     = 86400
)

type serviceKey struct {
	network, namespace, name string
	port                     int
	udp                      bool
}

// serviceID names a service on a network.
type serviceID struct {
	network, namespace, name string
}

// serviceListener serves one port of a service on loopback, carrying each
// connection, or each client's UDP flow, to the next of its endpoints.
type serviceListener struct {
	udp bool
	l   net.Listener
	// pc and flows serve a UDP port.
	pc    net.PacketConn
	flows io.Closer
	mu    sync.Mutex
	svc   Service
	eps   []netip.AddrPort
	n     int
	// sticky is the endpoint each client last reached, under ClientIP
	// affinity.
	sticky map[netip.Addr]stickyEndpoint
	// clients serve the port apart to each client address on the
	// service's network under ClientIP affinity: the engine tells a
	// stack's members, which reach the port through their router, by the
	// listener the stack's network file names. One outlives the service's
	// affinity, as a router keeps the UDP relay it made to it, until no
	// container holds the address or the port goes. The services' lock
	// guards it.
	clients map[netip.Addr]*clientListener
	// node serves the port on a node port as well, where one is set.
	node *nodeListener
}

// nodeListener serves a service port on one of the node's ports: l a TCP
// one, pc and flows a UDP one.
type nodeListener struct {
	port  int
	l     net.Listener
	pc    net.PacketConn
	flows io.Closer
}

func (n *nodeListener) close() {
	if n.l != nil {
		n.l.Close()
	}
	if n.flows != nil {
		n.flows.Close()
	}
	if n.pc != nil {
		n.pc.Close()
	}
}

// nodePort is the node port the service port is served on as well, or 0.
func (sl *serviceListener) nodePort() int {
	if sl.node == nil {
		return 0
	}
	return sl.node.port
}

type stickyEndpoint struct {
	ep   netip.AddrPort
	last time.Time
}

// clientListener serves a service port to one client address.
type clientListener struct {
	l     net.Listener
	pc    net.PacketConn
	flows io.Closer
}

func (cl *clientListener) port() int {
	if cl.pc != nil {
		return cl.pc.LocalAddr().(*net.UDPAddr).Port
	}
	return cl.l.Addr().(*net.TCPAddr).Port
}

func (cl *clientListener) close() {
	if cl.flows != nil {
		cl.flows.Close()
	}
	if cl.pc != nil {
		cl.pc.Close()
	}
	if cl.l != nil {
		cl.l.Close()
	}
}

// port is the loopback port the service port is served on.
func (sl *serviceListener) port() int {
	if sl.pc != nil {
		return sl.pc.LocalAddr().(*net.UDPAddr).Port
	}
	return sl.l.Addr().(*net.TCPAddr).Port
}

func (sl *serviceListener) close() {
	if sl.pc != nil {
		sl.flows.Close()
		sl.pc.Close()
	} else {
		sl.l.Close()
	}
	sl.closeNode()
	sl.closeClients()
}

func (sl *serviceListener) closeNode() {
	if sl.node != nil {
		sl.node.close()
		sl.node = nil
	}
}

func (sl *serviceListener) closeClients() {
	for a, cl := range sl.clients {
		cl.close()
		delete(sl.clients, a)
	}
}

// services holds the services of each network.
type services struct {
	mu        sync.Mutex
	byNetwork map[string][]Service
	listeners map[serviceKey]*serviceListener
	// member is the member address of each service without an IPv4
	// cluster IP, which the container library needs on every member's
	// line: an address of the member block no container takes.
	member map[serviceID]string
}

// clusterIPs are the service's cluster IPs, ClusterIP first; none for an
// ExternalName service.
func (s Service) clusterIPs() []netip.Addr {
	if s.ExternalName != "" {
		return nil
	}
	out := []netip.Addr{s.ClusterIP}
	for _, a := range s.ClusterIPs {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// addresses are every address the service answers at: its cluster IPs,
// then its external IPs.
func (s Service) addresses() []netip.Addr {
	out := s.clusterIPs()
	for _, a := range s.ExternalIPs {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// clusterIPv4 is the service's IPv4 cluster IP, if it has one.
func (s Service) clusterIPv4() (netip.Addr, bool) {
	for _, a := range s.clusterIPs() {
		if a.Is4() {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// affinity is how long the service keeps a client's endpoint, 0 for no
// affinity.
func (s Service) affinity() time.Duration {
	if !s.SessionAffinity.clientIP() {
		return 0
	}
	if s.AffinitySeconds == 0 {
		return defaultAffinity
	}
	return time.Duration(s.AffinitySeconds) * time.Second
}

// serviceAddr refuses an address a service cannot take.
func serviceAddr(s Service, what string, a netip.Addr) error {
	if !a.IsValid() || a.Zone() != "" || a.Is4In6() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() {
		return invalid("service %s/%s: %s %v is not a unicast IPv4 or IPv6 address", s.Namespace, s.Name, what, a)
	}
	return nil
}

// dnsName reports whether name is a DNS name: dot-separated labels of
// letters, digits, hyphens and underscores, with a final dot or without.
func dnsName(name string) bool {
	name = strings.TrimSuffix(name, ".")
	if name == "" || len(name) > 253 {
		return false
	}
	for _, l := range strings.Split(name, ".") {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return false
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// checkService refuses a service the engine cannot carry.
func checkService(s Service) error {
	if s.Name == "" || s.Namespace == "" || strings.ContainsAny(s.Name+s.Namespace, " \t\n./") {
		return invalid("a service needs a name and a namespace without spaces, dots or slashes: %q in %q", s.Name, s.Namespace)
	}
	if s.ExternalName != "" {
		if !dnsName(s.ExternalName) {
			return invalid("service %s/%s: external name %q is not a DNS name", s.Namespace, s.Name, s.ExternalName)
		}
		if s.ClusterIP.IsValid() || len(s.ClusterIPs) > 0 || len(s.ExternalIPs) > 0 || len(s.Ports) > 0 || s.affinity() > 0 {
			return invalid("service %s/%s: a service with an external name has no addresses, ports or session affinity", s.Namespace, s.Name)
		}
		return nil
	}
	cluster := s.clusterIPs()
	for _, a := range cluster {
		if err := serviceAddr(s, "cluster IP", a); err != nil {
			return err
		}
	}
	if len(cluster) > 2 || (len(cluster) == 2 && cluster[0].Is4() == cluster[1].Is4()) {
		return invalid("service %s/%s: cluster IPs %v: a service has one IPv4 and one IPv6 cluster IP at most", s.Namespace, s.Name, cluster)
	}
	for _, a := range s.ExternalIPs {
		if err := serviceAddr(s, "external IP", a); err != nil {
			return err
		}
		if !slices.ContainsFunc(cluster, func(c netip.Addr) bool { return c.Is4() == a.Is4() }) {
			return invalid("service %s/%s: external IP %v: the service has no cluster IP of its family", s.Namespace, s.Name, a)
		}
	}
	if a := s.SessionAffinity; !a.valid() {
		return invalid("service %s/%s: session affinity %q is neither None nor ClientIP", s.Namespace, s.Name, a)
	}
	if s.AffinitySeconds < 0 || s.AffinitySeconds > maxAffinity {
		return invalid("service %s/%s: a session affinity timeout of %d seconds is out of range", s.Namespace, s.Name, s.AffinitySeconds)
	}
	for _, p := range s.Ports {
		if p.Port < 1 || p.Port > 65535 || p.NodePort < 0 || p.NodePort > 65535 {
			return invalid("service %s/%s: port %d (node port %d) is out of range", s.Namespace, s.Name, p.Port, p.NodePort)
		}
		if !p.Protocol.valid() {
			return unsupported("service %s/%s: port %d/%s: only TCP and UDP ports are supported", s.Namespace, s.Name, p.Port, p.Protocol)
		}
		for _, ep := range p.Endpoints {
			if !ep.IsValid() || ep.Port() == 0 {
				return invalid("service %s/%s: port %d: endpoint %v is not an address and port", s.Namespace, s.Name, p.Port, ep)
			}
		}
	}
	return nil
}

// SetServices replaces the services on network: its members see each at
// its cluster IPs under its names, and at its external IPs, or an
// ExternalName service's names as aliases of its external name; and
// connections to a port reach the port's endpoints by turns, or a
// client's own under session affinity.
func (e *Engine) SetServices(network string, svcs []Service) error {
	ips := map[netip.Addr]string{}
	nodePorts := map[string]string{}
	names := map[string]bool{}
	for _, s := range svcs {
		if err := checkService(s); err != nil {
			return err
		}
		if names[s.Namespace+"/"+s.Name] {
			return invalid("service %s/%s is listed twice", s.Namespace, s.Name)
		}
		names[s.Namespace+"/"+s.Name] = true
		for _, a := range s.addresses() {
			if o, ok := ips[a]; ok {
				return invalid("services %s and %s/%s share address %s", o, s.Namespace, s.Name, a)
			}
			ips[a] = s.Namespace + "/" + s.Name
		}
		for _, p := range s.Ports {
			if p.NodePort == 0 {
				continue
			}
			np := fmt.Sprintf("%d/%t", p.NodePort, p.udp())
			if o, ok := nodePorts[np]; ok {
				return invalid("services %s and %s/%s share node port %d", o, s.Namespace, s.Name, p.NodePort)
			}
			nodePorts[np] = s.Namespace + "/" + s.Name
		}
	}
	e.mu.Lock()
	_, err := e.networkLocked(network)
	taken := map[string]bool{}
	for _, c := range e.containers {
		taken[c.rec.Address] = true
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	sv := &e.svcs
	sv.mu.Lock()
	err = e.setServicesLocked(network, svcs, taken)
	sv.mu.Unlock()
	if err != nil {
		return err
	}
	return e.rewriteNetworks()
}

// nodeID names a node port: its number and whether it is UDP's.
type nodeID struct {
	port int
	udp  bool
}

// setServicesLocked replaces network's services, under the services' lock,
// all at once or not at all: what the update opens, it opens before it
// closes anything, so an update that fails, on a node port another
// process holds, say, leaves the network's services as they were. A node
// port one of the network's ports gives up and another takes in the same
// update is the exception, which can only move once its holder lets it go:
// if it fails to, the ports it moved from have theirs back.
func (e *Engine) setServicesLocked(network string, svcs []Service, taken map[string]bool) error {
	sv := &e.svcs
	if sv.byNetwork == nil {
		sv.byNetwork, sv.listeners, sv.member = map[string][]Service{}, map[serviceKey]*serviceListener{}, map[serviceID]string{}
	}
	member, err := sv.assignMembers(network, svcs, taken)
	if err != nil {
		return err
	}
	want := map[serviceKey]int{}
	for _, s := range svcs {
		for _, p := range s.Ports {
			want[serviceKey{network, s.Namespace, s.Name, p.Port, p.udp()}] = p.NodePort
		}
	}
	// The network's node ports this update takes from the port holding
	// them, as that port goes or moves to another node port.
	released := map[nodeID]*serviceListener{}
	for k, sl := range sv.listeners {
		if k.network != network || sl.node == nil {
			continue
		}
		if np, ok := want[k]; !ok || np != sl.node.port {
			released[nodeID{sl.node.port, sl.udp}] = sl
		}
	}

	// The update's new listeners, and its new node ports but the
	// released ones: nothing the network serves changes yet.
	fresh := map[serviceKey]*serviceListener{}
	nodes := map[serviceKey]*nodeListener{}
	undo := func() {
		for _, n := range nodes {
			n.close()
		}
		for _, sl := range fresh {
			sl.close()
		}
	}
	type move struct {
		k serviceKey
		s Service
		p ServicePort
	}
	var moves []move
	for _, s := range svcs {
		for _, p := range s.Ports {
			k := serviceKey{network, s.Namespace, s.Name, p.Port, p.udp()}
			sl := sv.listeners[k]
			if sl == nil {
				if sl, err = e.listenService(network, p.udp()); err != nil {
					undo()
					return err
				}
				fresh[k] = sl
			}
			if p.NodePort == 0 || sl.nodePort() == p.NodePort {
				continue
			}
			if released[nodeID{p.NodePort, p.udp()}] != nil {
				moves = append(moves, move{k, s, p})
				continue
			}
			n, err := e.listenServiceOnNode(network, sl, p.NodePort)
			if err != nil {
				undo()
				return fmt.Errorf("service %s/%s: node port %d/%s: %w", s.Namespace, s.Name, p.NodePort, p.proto(), err)
			}
			nodes[k] = n
		}
	}

	// The released node ports move: each holder lets its port go, and the
	// port that takes it listens on it. A move that fails puts back the
	// ports moved so far.
	type held struct {
		sl   *serviceListener
		port int
	}
	var given []held
	putBack := func() {
		for _, h := range given {
			n, err := e.listenServiceOnNode(network, h.sl, h.port)
			if err != nil {
				log.Printf("services on %s: node port %d: %v", network, h.port, err)
				continue
			}
			h.sl.node = n
		}
	}
	for _, m := range moves {
		id := nodeID{m.p.NodePort, m.p.udp()}
		if holder := released[id]; holder.node != nil && holder.node.port == id.port {
			holder.closeNode()
			given = append(given, held{holder, id.port})
		}
		sl := sv.listeners[m.k]
		if sl == nil {
			sl = fresh[m.k]
		}
		n, err := e.listenServiceOnNode(network, sl, m.p.NodePort)
		if err != nil {
			undo()
			putBack()
			return fmt.Errorf("service %s/%s: node port %d/%s: %w", m.s.Namespace, m.s.Name, m.p.NodePort, m.p.proto(), err)
		}
		nodes[m.k] = n
	}

	// Nothing fails from here: the ports that go close, and the rest take
	// their new node ports, services and endpoints.
	for k, sl := range sv.listeners {
		if k.network != network {
			continue
		}
		switch np, ok := want[k]; {
		case !ok:
			sl.close()
			delete(sv.listeners, k)
		case sl.nodePort() != np:
			sl.closeNode()
		}
	}
	for k, sl := range fresh {
		sv.listeners[k] = sl
	}
	for k, n := range nodes {
		sv.listeners[k].node = n
	}
	for _, s := range svcs {
		for _, p := range s.Ports {
			sl := sv.listeners[serviceKey{network, s.Namespace, s.Name, p.Port, p.udp()}]
			sl.mu.Lock()
			if s.affinity() != sl.svc.affinity() {
				sl.sticky = nil
			}
			sl.svc, sl.eps = s, append([]netip.AddrPort(nil), p.Endpoints...)
			sl.mu.Unlock()
		}
	}
	sv.member = member
	if len(svcs) == 0 {
		delete(sv.byNetwork, network)
	} else {
		sv.byNetwork[network] = append([]Service(nil), svcs...)
	}
	return nil
}

// listenService opens a loopback listener for a service port of network,
// TCP or UDP, and serves it.
func (e *Engine) listenService(network string, udp bool) (*serviceListener, error) {
	if udp {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		sl := &serviceListener{udp: true, pc: pc}
		sl.flows = portproxy.ServeUDPFlows(pc, func(c *net.UDPAddr) (net.Conn, error) {
			return e.dialEndpointUDP(network, sl, clientAddr(c))
		}, nil)
		return sl, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	sl := &serviceListener{l: l}
	go e.serveService(network, sl, l, remoteAddr)
	return sl, nil
}

// assignMembers gives each of network's services without an IPv4 cluster
// IP a member address, keeping the one it holds, from the top of the
// member block down, where containers take theirs from the bottom up; it
// frees those of the network's services that are gone. It returns the
// services' member addresses after the update, leaving sv's as they are.
func (sv *services) assignMembers(network string, svcs []Service, taken map[string]bool) (map[serviceID]string, error) {
	need := map[serviceID]bool{}
	for _, s := range svcs {
		if _, ok := s.clusterIPv4(); !ok && s.ExternalName == "" {
			need[serviceID{network, s.Namespace, s.Name}] = true
		}
	}
	member := map[serviceID]string{}
	for id, a := range sv.member {
		if id.network == network && !need[id] {
			continue
		}
		member[id], taken[a] = a, true
		delete(need, id)
	}
	ids := make([]serviceID, 0, len(need))
	for id := range need {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].namespace+"/"+ids[i].name < ids[j].namespace+"/"+ids[j].name })
	i := 1<<16 - 2
	for _, id := range ids {
		for ; i >= 2 && taken[fmt.Sprintf("%s%d.%d", memberBlock, i>>8, i&255)]; i-- {
		}
		if i < 2 {
			return nil, fmt.Errorf("service %s/%s: no member address is free", id.namespace, id.name)
		}
		a := fmt.Sprintf("%s%d.%d", memberBlock, i>>8, i&255)
		member[id], taken[a] = a, true
	}
	return member, nil
}

// listenServiceOnNode serves sl's port on the node's port p as well.
func (e *Engine) listenServiceOnNode(network string, sl *serviceListener, p int) (*nodeListener, error) {
	if sl.udp {
		pc, err := e.listenUDPOnNode(p)
		if err != nil {
			return nil, err
		}
		return &nodeListener{port: p, pc: pc, flows: portproxy.ServeUDPFlows(pc, func(c *net.UDPAddr) (net.Conn, error) {
			return e.dialEndpointUDP(network, sl, clientAddr(c))
		}, nil)}, nil
	}
	nl, err := e.listenOnNode(p)
	if err != nil {
		return nil, err
	}
	go e.serveService(network, sl, nl, remoteAddr)
	return &nodeListener{port: p, l: nl}, nil
}

// Services are the services on network.
func (e *Engine) Services(network string) []Service {
	e.svcs.mu.Lock()
	defer e.svcs.mu.Unlock()
	return append([]Service(nil), e.svcs.byNetwork[network]...)
}

// closeServices stops serving every service.
func (e *Engine) closeServices() {
	e.svcs.mu.Lock()
	defer e.svcs.mu.Unlock()
	for k, sl := range e.svcs.listeners {
		sl.close()
		delete(e.svcs.listeners, k)
	}
	e.svcs.byNetwork, e.svcs.member = nil, nil
}

// remoteAddr is the address a connection's client is at.
func remoteAddr(c net.Conn) netip.Addr {
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

// clientAddr is the address of a UDP client.
func clientAddr(c *net.UDPAddr) netip.Addr {
	return c.AddrPort().Addr().Unmap()
}

// serveService carries each connection l accepts to an endpoint, picked
// for the client client names.
func (e *Engine) serveService(network string, sl *serviceListener, l net.Listener, client func(net.Conn) netip.Addr) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			up, err := e.dialEndpoint(network, sl, client(c))
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(up, c); closeWriteConn(up); done <- struct{}{} }()
			go func() { io.Copy(c, up); closeWriteConn(c); done <- struct{}{} }()
			<-done
			<-done
		}()
	}
}

func closeWriteConn(c net.Conn) {
	if h, ok := c.(interface{ CloseWrite() error }); ok {
		h.CloseWrite()
		return
	}
	c.Close()
}

// pick opens a connection or flow to the service port's endpoints by
// turns, trying the next where one refuses, as a client retries a
// service. Under ClientIP affinity, client's endpoint comes first while it
// is one of the port's and the client has not been away longer than the
// timeout, and the endpoint reached becomes client's.
func (sl *serviceListener) pick(client netip.Addr, dial func(netip.AddrPort) (net.Conn, error)) (net.Conn, error) {
	sl.mu.Lock()
	eps := sl.eps
	start := sl.n
	sl.n++
	keep := sl.svc.affinity()
	sticky := keep > 0 && client.IsValid()
	var mine netip.AddrPort
	if sticky {
		now := time.Now()
		for c, s := range sl.sticky {
			if now.Sub(s.last) >= keep {
				delete(sl.sticky, c)
			}
		}
		mine = sl.sticky[client].ep
	}
	sl.mu.Unlock()
	order := make([]netip.AddrPort, 0, len(eps))
	if mine.IsValid() && slices.Contains(eps, mine) {
		order = append(order, mine)
	}
	for i := range eps {
		if ep := eps[(start+i)%len(eps)]; ep != mine {
			order = append(order, ep)
		}
	}
	last := errors.New("no endpoints")
	for _, ep := range order {
		c, err := dial(ep)
		if err != nil {
			last = err
			continue
		}
		if sticky {
			sl.mu.Lock()
			if sl.sticky == nil {
				sl.sticky = map[netip.Addr]stickyEndpoint{}
			}
			sl.sticky[client] = stickyEndpoint{ep, time.Now()}
			sl.mu.Unlock()
		}
		return c, nil
	}
	return nil, last
}

// dialEndpoint connects client to one of the service port's endpoints.
func (e *Engine) dialEndpoint(network string, sl *serviceListener, client netip.Addr) (net.Conn, error) {
	return sl.pick(client, func(ep netip.AddrPort) (net.Conn, error) { return e.dialMember(network, ep) })
}

// udp reports whether the port is a UDP one.
func (p ServicePort) udp() bool { return p.Protocol.udp() }

func (p ServicePort) proto() string {
	if p.udp() {
		return "udp"
	}
	return "tcp"
}

// dialEndpointUDP opens client's flow to one of the service port's
// endpoints, from a port of the endpoint's stack's block, which the
// endpoint's sandbox lets it answer: to its UDP host port, or its router's
// flow port for one without.
func (e *Engine) dialEndpointUDP(network string, sl *serviceListener, client netip.Addr) (net.Conn, error) {
	return sl.pick(client, func(ep netip.AddrPort) (net.Conn, error) { return e.dialMemberUDP(network, ep) })
}

func (e *Engine) dialMemberUDP(network string, ep netip.AddrPort) (net.Conn, error) {
	e.mu.Lock()
	var hp, rp int
	var stack string
	found := false
	for _, c := range e.containers {
		es := c.rec.Networks[network]
		if es == nil || (es.IPAddress != ep.Addr() && es.GlobalIPv6Address != ep.Addr()) || !c.isRunning() {
			continue
		}
		found = true
		hp, rp, stack = c.rec.Ports[fmt.Sprintf("%d/udp", ep.Port())], c.rec.RouterPort, c.rec.Stack
		break
	}
	e.mu.Unlock()
	switch {
	case !found && e.cfg.Nested != nil && e.cfg.Nested.DialUDP != nil:
		// An endpoint on another node, as cluster DNS's often is.
		return e.cfg.Nested.DialUDP(ep)
	case !found:
		return nil, fmt.Errorf("no running member of %s is at %s", network, ep.Addr())
	}
	up, err := udpUpstream(hp, rp, ep)
	if err != nil {
		return nil, err
	}
	return alloc.DialFlow(e.cfg.Root, stack, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: up})
}

// dialMember connects to port ep.Port of the member at address ep.Addr on
// network: at its host port, or through its router. An engine nested in
// a privileged container reaches an address none of its members holds on
// that container's networks, as a cluster's API server is reached.
func (e *Engine) dialMember(network string, ep netip.AddrPort) (net.Conn, error) {
	e.mu.Lock()
	var hp, rp int
	found := false
	for _, c := range e.containers {
		es := c.rec.Networks[network]
		if es == nil || (es.IPAddress != ep.Addr() && es.GlobalIPv6Address != ep.Addr()) || !c.isRunning() {
			continue
		}
		found = true
		hp, rp = c.rec.Ports[fmt.Sprintf("%d/tcp", ep.Port())], c.rec.RouterPort
		break
	}
	e.mu.Unlock()
	switch {
	case !found && e.cfg.Nested != nil && e.cfg.Nested.Dial != nil:
		return e.cfg.Nested.Dial(ep)
	case !found:
		return nil, fmt.Errorf("no running member of %s is at %s", network, ep.Addr())
	case hp != 0:
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
	case rp != 0:
		return router.Dial(net.JoinHostPort("127.0.0.1", strconv.Itoa(rp)), int(ep.Port()))
	}
	return nil, fmt.Errorf("%s does not listen on port %d", ep.Addr(), ep.Port())
}

// clientPort is the loopback port that serves the service port to the
// client at address client alone, opened the first time.
func (e *Engine) clientPort(network string, sl *serviceListener, client netip.Addr) (int, error) {
	if cl := sl.clients[client]; cl != nil {
		return cl.port(), nil
	}
	cl := &clientListener{}
	if sl.udp {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		cl.pc = pc
		cl.flows = portproxy.ServeUDPFlows(pc, func(*net.UDPAddr) (net.Conn, error) {
			return e.dialEndpointUDP(network, sl, client)
		}, nil)
	} else {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		cl.l = l
		go e.serveService(network, sl, l, func(net.Conn) netip.Addr { return client })
	}
	if sl.clients == nil {
		sl.clients = map[netip.Addr]*clientListener{}
	}
	sl.clients[client] = cl
	return cl.port(), nil
}

// pruneServiceClientsLocked stops serving service ports to client
// addresses no container holds on the services' networks any more.
func (e *Engine) pruneServiceClientsLocked() {
	held := map[string]map[netip.Addr]bool{}
	for _, c := range e.containers {
		for n, es := range c.rec.Networks {
			if es == nil {
				continue
			}
			if held[n] == nil {
				held[n] = map[netip.Addr]bool{}
			}
			held[n][es.IPAddress], held[n][es.GlobalIPv6Address] = true, true
		}
	}
	sv := &e.svcs
	sv.mu.Lock()
	defer sv.mu.Unlock()
	for k, sl := range sv.listeners {
		for a, cl := range sl.clients {
			if !held[k.network][a] {
				cl.close()
				delete(sl.clients, a)
			}
		}
	}
}

// serviceMembers are the network file's lines for the services on the
// networks a stack's members are on: each at its cluster IPs and external
// IPs, as another stack's member on the network, whose ports are its
// listeners'. On a network where the stack is at clients[network], a
// service with session affinity lists the ports that serve that address
// alone.
func (e *Engine) serviceMembers(nets map[string]bool, clients map[string]netip.Addr) []stackmembers.Member {
	sv := &e.svcs
	sv.mu.Lock()
	defer sv.mu.Unlock()
	var names []string
	for n := range sv.byNetwork {
		if nets[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []stackmembers.Member
	for _, n := range names {
		client := clients[n]
		for _, s := range sv.byNetwork[n] {
			svcNames := []string{s.Name + "." + s.Namespace + ".svc." + ClusterDomain, s.Name + "." + s.Namespace + ".svc", s.Name + "." + s.Namespace, s.Name}
			if s.ExternalName != "" {
				out = append(out, stackmembers.Member{Alias: s.ExternalName, Names: svcNames, Networks: []string{n}, Foreign: true, Stack: stackmembers.ServicesStack})
				continue
			}
			ports := map[string]int{}
			for _, p := range s.Ports {
				sl := sv.listeners[serviceKey{n, s.Namespace, s.Name, p.Port, p.udp()}]
				if sl == nil {
					continue
				}
				port := sl.port()
				if s.affinity() > 0 && client.IsValid() {
					if cp, err := e.clientPort(n, sl, client); err == nil {
						port = cp
					}
				}
				ports[fmt.Sprintf("%d/%s", p.Port, p.proto())] = port
			}
			address, ok := s.clusterIPv4()
			member := address.String()
			if !ok {
				member = sv.member[serviceID{n, s.Namespace, s.Name}]
			}
			// The container library resolves the names to the first
			// address of each family on the network: the cluster IPs
			// come before the external IPs.
			var subnets []stackmembers.NetAddress
			for _, a := range s.addresses() {
				subnets = append(subnets, stackmembers.NetAddress{Network: n, Address: netip.PrefixFrom(a, a.BitLen())})
			}
			sm := stackmembers.Member{
				Address: member, Names: svcNames, Ports: ports, Networks: []string{n}, Foreign: true, Stack: stackmembers.ServicesStack,
				Subnets: subnets,
			}
			// A service with no IPv4 cluster IP answers over IPv6 alone.
			if !ok {
				sm.Only6 = []string{n}
			}
			out = append(out, sm)
		}
	}
	return out
}
