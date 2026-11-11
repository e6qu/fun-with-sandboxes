package engine

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"

	stackmembers "github.com/e6qu/fun-with-sandboxes/internal/members"
	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// Service is a virtual address on a network, as a Kubernetes Service's
// cluster IP is: its members reach it by address and by name, and the
// engine carries each connection to one of its endpoints, as kube-proxy's
// userspace mode does.
type Service struct {
	// Name and Namespace give the service's names: name.namespace,
	// name.namespace.svc and name.namespace.svc.<domain>, and name alone.
	Name      string        `json:"name"`
	Namespace string        `json:"namespace"`
	ClusterIP netip.Addr    `json:"clusterIP"`
	Ports     []ServicePort `json:"ports"`
}

// ServicePort is a TCP port of a service and the endpoints behind it: the
// addresses of members on the service's network, and their ports.
type ServicePort struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	// NodePort, when set, is the port of the engine's node where the
	// service port answers too, as a NodePort Service's does: for an
	// engine nested in a privileged container, that container's port,
	// which the members of its networks reach.
	NodePort  int              `json:"nodePort,omitempty"`
	Endpoints []netip.AddrPort `json:"endpoints"`
}

// ClusterDomain is the domain of the services' names.
const ClusterDomain = "cluster.local"

// servicesStack names the services in the network files' stack column.
const servicesStack = "services"

type serviceKey struct {
	network, namespace, name string
	port                     int
}

// serviceListener serves one port of a service on loopback, carrying each
// connection to the next of its endpoints.
type serviceListener struct {
	l   net.Listener
	svc Service
	mu  sync.Mutex
	eps []netip.AddrPort
	n   int
	// node listens on the node's port nodePort, where one is set.
	node     net.Listener
	nodePort int
}

func (sl *serviceListener) close() {
	sl.l.Close()
	if sl.node != nil {
		sl.node.Close()
	}
}

// services holds the services of each network.
type services struct {
	mu        sync.Mutex
	byNetwork map[string][]Service
	listeners map[serviceKey]*serviceListener
}

// checkService refuses a service the engine cannot carry.
func checkService(s Service) error {
	if s.Name == "" || s.Namespace == "" || strings.ContainsAny(s.Name+s.Namespace, " \t\n./") {
		return invalid("a service needs a name and a namespace without spaces, dots or slashes: %q in %q", s.Name, s.Namespace)
	}
	if !s.ClusterIP.Is4() {
		return invalid("service %s/%s: its cluster IP %v is not an IPv4 address", s.Namespace, s.Name, s.ClusterIP)
	}
	for _, p := range s.Ports {
		if p.Port < 1 || p.Port > 65535 || p.NodePort < 0 || p.NodePort > 65535 {
			return invalid("service %s/%s: port %d (node port %d) is out of range", s.Namespace, s.Name, p.Port, p.NodePort)
		}
		if p.Protocol != "" && !strings.EqualFold(p.Protocol, "tcp") {
			return unsupported("service %s/%s: port %d/%s: only TCP ports are supported", s.Namespace, s.Name, p.Port, p.Protocol)
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
// its cluster IP under its names, and connections to a port reach the
// port's endpoints by turns.
func (e *Engine) SetServices(network string, svcs []Service) error {
	e.mu.Lock()
	_, err := e.networkLocked(network)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	ips := map[netip.Addr]string{}
	for _, s := range svcs {
		if err := checkService(s); err != nil {
			return err
		}
		if o, ok := ips[s.ClusterIP]; ok {
			return invalid("services %s and %s/%s share cluster IP %s", o, s.Namespace, s.Name, s.ClusterIP)
		}
		ips[s.ClusterIP] = s.Namespace + "/" + s.Name
	}
	sv := &e.svcs
	sv.mu.Lock()
	if sv.byNetwork == nil {
		sv.byNetwork, sv.listeners = map[string][]Service{}, map[serviceKey]*serviceListener{}
	}
	want := map[serviceKey]bool{}
	for _, s := range svcs {
		for _, p := range s.Ports {
			k := serviceKey{network, s.Namespace, s.Name, p.Port}
			want[k] = true
			sl := sv.listeners[k]
			if sl == nil {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					sv.mu.Unlock()
					return err
				}
				sl = &serviceListener{l: l}
				sv.listeners[k] = sl
				go e.serveService(network, sl, l)
			}
			if sl.nodePort != p.NodePort {
				if sl.node != nil {
					sl.node.Close()
					sl.node, sl.nodePort = nil, 0
				}
				if p.NodePort != 0 {
					nl, err := e.listenOnNode(p.NodePort)
					if err != nil {
						sv.mu.Unlock()
						return fmt.Errorf("service %s/%s: node port %d: %w", s.Namespace, s.Name, p.NodePort, err)
					}
					sl.node, sl.nodePort = nl, p.NodePort
					go e.serveService(network, sl, nl)
				}
			}
			sl.mu.Lock()
			sl.svc, sl.eps = s, append([]netip.AddrPort(nil), p.Endpoints...)
			sl.mu.Unlock()
		}
	}
	for k, sl := range sv.listeners {
		if k.network == network && !want[k] {
			sl.close()
			delete(sv.listeners, k)
		}
	}
	if len(svcs) == 0 {
		delete(sv.byNetwork, network)
	} else {
		sv.byNetwork[network] = append([]Service(nil), svcs...)
	}
	sv.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	stacks := map[string]bool{}
	for _, c := range e.containers {
		stacks[c.rec.Stack] = true
	}
	for st := range stacks {
		if err := e.writeStackNetworkLocked(st); err != nil {
			return err
		}
	}
	return nil
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
	e.svcs.byNetwork = nil
}

func (e *Engine) serveService(network string, sl *serviceListener, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			up, err := e.dialEndpoint(network, sl)
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

// dialEndpoint connects to the service port's endpoints by turns, trying
// the next where one refuses, as a client retries a service.
func (e *Engine) dialEndpoint(network string, sl *serviceListener) (net.Conn, error) {
	sl.mu.Lock()
	eps := sl.eps
	start := sl.n
	sl.n++
	sl.mu.Unlock()
	last := fmt.Errorf("no endpoints")
	for i := range eps {
		ep := eps[(start+i)%len(eps)]
		c, err := e.dialMember(network, ep)
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
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
		if es == nil || es.IPAddress != ep.Addr() || !c.isRunning() {
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

// serviceMembers are the network file's lines for the services on the
// networks a stack's members are on: each at its cluster IP, as another
// stack's member on the network, whose ports are its listeners'.
func (e *Engine) serviceMembers(nets map[string]bool) []stackmembers.Member {
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
		for _, s := range sv.byNetwork[n] {
			ports := map[string]int{}
			for _, p := range s.Ports {
				if sl := sv.listeners[serviceKey{n, s.Namespace, s.Name, p.Port}]; sl != nil {
					ports[fmt.Sprintf("%d/tcp", p.Port)] = sl.l.Addr().(*net.TCPAddr).Port
				}
			}
			out = append(out, stackmembers.Member{
				Address: s.ClusterIP.String(),
				Names: []string{s.Name + "." + s.Namespace + ".svc." + ClusterDomain, s.Name + "." + s.Namespace + ".svc",
					s.Name + "." + s.Namespace, s.Name},
				Ports: ports, Networks: []string{n}, Foreign: true, Stack: servicesStack,
				Subnets: []stackmembers.NetAddress{{Network: n, Address: netip.PrefixFrom(s.ClusterIP, 32)}},
			})
		}
	}
	return out
}
