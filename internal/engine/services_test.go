package engine

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/alloc"
	stackmembers "github.com/e6qu/fun-with-sandboxes/internal/members"
	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// A service on a network: its members see it at its cluster IP under its
// names, and its port carries connections to its endpoints by turns.
func TestServices(t *testing.T) {
	a, err := alloc.Open(alloc.Config{Root: t.TempDir(), PortBase: 30000, PortLimit: 31000, BlockSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{alloc: a, containers: map[string]*Container{}, networks: map[string]*Network{
		"n1": {ID: "n1", Name: "pods", Driver: "bridge", Pool: Pool{Subnet: "10.244.0.0/24", Gateway: "10.244.0.1"}},
	}}
	defer e.closeServices()
	pod := func(stack, ip, body string) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		t.Cleanup(srv.Close)
		port := srv.Listener.Addr().(*net.TCPAddr).Port
		addr, err := e.assignAddressLocked(stack)
		if err != nil {
			t.Fatal(err)
		}
		e.containers[stack] = &Container{rec: record{ID: stack, Name: stack, Stack: stack, Address: addr,
			Ports:    map[string]int{"8080/tcp": port},
			State:    stateRec{Status: container.StateRunning},
			Networks: map[string]*network.EndpointSettings{"pods": {IPAddress: netip.MustParseAddr(ip), IPPrefixLen: 24}}}}
	}
	pod("pod-a", "10.244.0.5", "a")
	pod("pod-b", "10.244.0.6", "b")
	svc := Service{Name: "web", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.10"), Ports: []ServicePort{{Port: 80, Protocol: "TCP",
		Endpoints: []netip.AddrPort{netip.MustParseAddrPort("10.244.0.5:8080"), netip.MustParseAddrPort("10.244.0.6:8080")}}}}
	if err := e.SetServices("pods", []Service{svc}); err != nil {
		t.Fatal(err)
	}
	read := func() []stackmembers.Member {
		path, _ := e.networkFile("pod-a")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return stackmembers.Parse(b)
	}
	m, ok := stackmembers.Find(read(), "web.default.svc.cluster.local")
	if !ok || m.Address != "10.96.0.10" || !m.Foreign || !m.Has("10.96.0.10") {
		t.Fatalf("the service's line: %+v, %v", m, ok)
	}
	for _, n := range []string{"web", "web.default", "web.default.svc"} {
		if _, ok := stackmembers.Find(read(), n); !ok {
			t.Errorf("the service does not answer to %s", n)
		}
	}
	hp := m.Ports["80/tcp"]
	if hp == 0 {
		t.Fatalf("the service's port 80 has no listener: %+v", m.Ports)
	}
	// A connection apiece, as the service balances connections.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	get := func() string {
		res, err := client.Get("http://127.0.0.1:" + strconv.Itoa(hp) + "/")
		if err != nil {
			return err.Error()
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	if got := get() + get() + get() + get(); got != "abab" && got != "baba" {
		t.Errorf("the endpoints answered %q, want them by turns", got)
	}

	// An endpoint no member holds is passed over.
	svc.Ports[0].Endpoints = []netip.AddrPort{netip.MustParseAddrPort("10.244.0.9:8080"), netip.MustParseAddrPort("10.244.0.6:8080")}
	if err := e.SetServices("pods", []Service{svc}); err != nil {
		t.Fatal(err)
	}
	if m2, _ := stackmembers.Find(read(), "web"); m2.Ports["80/tcp"] != hp {
		t.Errorf("an updated service moved its listener from %d to %d", hp, m2.Ports["80/tcp"])
	}
	if got := get() + get(); got != "bb" {
		t.Errorf("with one endpoint gone: %q", got)
	}

	// A node port answers on the node: the host's loopback, for an engine
	// that is not nested.
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	nodePort := free.Addr().(*net.TCPAddr).Port
	free.Close()
	svc.Ports[0].NodePort = nodePort
	if err := e.SetServices("pods", []Service{svc}); err != nil {
		t.Fatal(err)
	}
	if res, err := client.Get("http://127.0.0.1:" + strconv.Itoa(nodePort) + "/"); err != nil {
		t.Errorf("the node port: %v", err)
	} else {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(b) != "b" {
			t.Errorf("the node port answered %q", b)
		}
	}
	svc.Ports[0].NodePort = 0
	if err := e.SetServices("pods", []Service{svc}); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(nodePort)); err == nil {
		c.Close()
		t.Error("a node port the service no longer has still listens")
	}

	if err := e.SetServices("pods", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := stackmembers.Find(read(), "web"); ok {
		t.Error("a removed service is still listed")
	}
	if _, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(hp)); err == nil {
		t.Error("a removed service's port still listens")
	}

	for name, bad := range map[string]Service{
		"no namespace": {Name: "web", ClusterIP: netip.MustParseAddr("10.96.0.11")},
		"two IPv6 cluster IPs": {Name: "web", Namespace: "default", ClusterIP: netip.MustParseAddr("fd00::1"),
			ClusterIPs: []netip.Addr{netip.MustParseAddr("fd00::2")}},
		"an IPv4-mapped IP": {Name: "web", Namespace: "default", ClusterIP: netip.MustParseAddr("::ffff:10.96.0.11")},
		"an SCTP port":      {Name: "dns", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.12"), Ports: []ServicePort{{Port: 53, Protocol: "SCTP"}}},
		"two IPv4 cluster IPs": {Name: "web", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.14"),
			ClusterIPs: []netip.Addr{netip.MustParseAddr("10.96.0.15")}},
		"an external IP of a family without a cluster IP": {Name: "web", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.14"),
			ExternalIPs: []netip.Addr{netip.MustParseAddr("2001:db8::1")}},
		"a loopback external IP": {Name: "web", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.14"),
			ExternalIPs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		"an unknown affinity":          {Name: "web", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.14"), SessionAffinity: "Cookie"},
		"an external name with a port": {Name: "ext", Namespace: "default", ExternalName: "db.example.com", Ports: []ServicePort{{Port: 80}}},
		"an external name with a cluster IP": {Name: "ext", Namespace: "default", ExternalName: "db.example.com",
			ClusterIP: netip.MustParseAddr("10.96.0.14")},
		"an external name that is no name": {Name: "ext", Namespace: "default", ExternalName: "db example"},
		"a day and more of affinity": {Name: "web", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.14"),
			SessionAffinity: "ClientIP", AffinitySeconds: 86401},
		"a dotted name": {Name: "a.b", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.13")},
	} {
		if err := e.SetServices("pods", []Service{bad}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, bad := range map[string][]Service{
		"an external IP that is another's cluster IP": {
			{Name: "a", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.11")},
			{Name: "b", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.12"), ExternalIPs: []netip.Addr{netip.MustParseAddr("10.96.0.11")}},
		},
		"a service twice": {
			{Name: "a", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.11")},
			{Name: "a", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.12")},
		},
		"a shared node port": {
			{Name: "a", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.11"), Ports: []ServicePort{{Port: 80, NodePort: 30080}}},
			{Name: "b", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.12"), Ports: []ServicePort{{Port: 80, NodePort: 30080}}},
		},
	} {
		if err := e.SetServices("pods", bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := e.SetServices("missing", nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a missing network: %v", err)
	}
}

// A nested engine reaches an endpoint none of its members holds through
// the engine it is nested in, as a cluster's API server is reached.
func TestServiceEndpointsBeyondTheNetwork(t *testing.T) {
	a, err := alloc.Open(alloc.Config{Root: t.TempDir(), PortBase: 30000, PortLimit: 31000, BlockSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "api") }))
	defer srv.Close()
	asked := make(chan netip.AddrPort, 1)
	e := &Engine{alloc: a, containers: map[string]*Container{}, networks: map[string]*Network{
		"n1": {ID: "n1", Name: "pods", Driver: "bridge", Pool: Pool{Subnet: "10.244.0.0/24", Gateway: "10.244.0.1"}},
	}, cfg: Config{Nested: &Nested{Dial: func(ap netip.AddrPort) (net.Conn, error) {
		asked <- ap
		return net.Dial("tcp", srv.Listener.Addr().String())
	}}}}
	defer e.closeServices()
	api := netip.MustParseAddrPort("172.30.7.3:6443")
	if err := e.SetServices("pods", []Service{{Name: "kubernetes", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.1"),
		Ports: []ServicePort{{Port: 443, Endpoints: []netip.AddrPort{api}}}}}); err != nil {
		t.Fatal(err)
	}
	var hp int
	for k, sl := range e.svcs.listeners {
		if k.name == "kubernetes" {
			hp = sl.l.Addr().(*net.TCPAddr).Port
		}
	}
	res, err := http.Get("http://127.0.0.1:" + strconv.Itoa(hp) + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if got := <-asked; string(b) != "api" || got != api {
		t.Errorf("got %q through %v, want the endpoint %v", b, got, api)
	}
}

// A UDP service port carries each client's datagrams to the next of its
// endpoints and the replies back, from a port of the endpoint's stack's
// block, as cluster DNS answers on port 53 beside its TCP port.
func TestUDPService(t *testing.T) {
	root := t.TempDir()
	a, err := alloc.Open(alloc.Config{Root: root, PortBase: 30000, PortLimit: 31000, BlockSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: Config{Root: root}, alloc: a, containers: map[string]*Container{}, networks: map[string]*Network{
		"n1": {ID: "n1", Name: "pods", Driver: "bridge", Pool: Pool{Subnet: "10.244.0.0/24", Gateway: "10.244.0.1"}},
	}}
	defer e.closeServices()
	pod := func(stack, ip string) {
		if _, err := a.Stack(stack); err != nil {
			t.Fatal(err)
		}
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pc.Close() })
		go func() {
			b := make([]byte, 512)
			for {
				n, from, err := pc.ReadFrom(b)
				if err != nil {
					return
				}
				pc.WriteTo([]byte(stack+":"+string(b[:n])), from)
			}
		}()
		addr, err := e.assignAddressLocked(stack)
		if err != nil {
			t.Fatal(err)
		}
		e.containers[stack] = &Container{rec: record{ID: stack, Name: stack, Stack: stack, Address: addr,
			Ports:    map[string]int{"5353/udp": pc.LocalAddr().(*net.UDPAddr).Port},
			State:    stateRec{Status: container.StateRunning},
			Networks: map[string]*network.EndpointSettings{"pods": {IPAddress: netip.MustParseAddr(ip), IPPrefixLen: 24}}}}
	}
	pod("dns-a", "10.244.0.5")
	pod("dns-b", "10.244.0.6")
	eps := []netip.AddrPort{netip.MustParseAddrPort("10.244.0.5:5353"), netip.MustParseAddrPort("10.244.0.6:5353")}
	svc := Service{Name: "kube-dns", Namespace: "kube-system", ClusterIP: netip.MustParseAddr("10.96.0.10"), Ports: []ServicePort{
		{Port: 53, Protocol: "UDP", Endpoints: eps}, {Port: 53, Protocol: "TCP", Endpoints: eps}}}
	if err := e.SetServices("pods", []Service{svc}); err != nil {
		t.Fatal(err)
	}
	path, _ := e.networkFile("dns-a")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := stackmembers.Find(stackmembers.Parse(b), "kube-dns.kube-system")
	if !ok || m.Ports["53/udp"] == 0 || m.Ports["53/tcp"] == 0 {
		t.Fatalf("the service's line: %+v, %v", m, ok)
	}
	if m.Stack != stackmembers.ServicesStack {
		t.Errorf("the service's stack is %q", m.Stack)
	}
	ask := func(port int) string {
		c, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("q"))
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil {
			return err.Error()
		}
		return string(buf[:n])
	}
	// A flow apiece, as the service balances clients.
	if got := ask(m.Ports["53/udp"]) + " " + ask(m.Ports["53/udp"]); got != "dns-a:q dns-b:q" && got != "dns-b:q dns-a:q" {
		t.Errorf("the endpoints answered %q, want them by turns", got)
	}

	// A UDP node port answers on the node: the host's loopback, for an
	// engine that is not nested.
	free, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	nodePort := free.LocalAddr().(*net.UDPAddr).Port
	free.Close()
	svc.Ports[0].NodePort = nodePort
	if err := e.SetServices("pods", []Service{svc}); err != nil {
		t.Fatal(err)
	}
	if got := ask(nodePort); got != "dns-a:q" && got != "dns-b:q" {
		t.Errorf("the node port answered %q", got)
	}
	svc.Ports[0].NodePort = 0
	if err := e.SetServices("pods", []Service{svc}); err != nil {
		t.Fatal(err)
	}
	if pc, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(nodePort)); err != nil {
		t.Errorf("a node port the service no longer has is still held: %v", err)
	} else {
		pc.Close()
	}
}

// Under ClientIP affinity, each stack's members reach a service port at a
// listener of their own, which tells the engine the client, and keep the
// endpoint they first reached while it serves and they come back within
// the timeout.
func TestServiceAffinity(t *testing.T) {
	a, err := alloc.Open(alloc.Config{Root: t.TempDir(), PortBase: 30000, PortLimit: 31000, BlockSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{alloc: a, containers: map[string]*Container{}, networks: map[string]*Network{
		"n1": {ID: "n1", Name: "pods", Driver: "bridge", Pool: Pool{Subnet: "10.244.0.0/24", Gateway: "10.244.0.1"}},
	}}
	defer e.closeServices()
	member := func(stack, ip string, ports map[string]int) {
		addr, err := e.assignAddressLocked(stack)
		if err != nil {
			t.Fatal(err)
		}
		e.containers[stack] = &Container{rec: record{ID: stack, Name: stack, Stack: stack, Address: addr, Ports: ports,
			State:    stateRec{Status: container.StateRunning},
			Networks: map[string]*network.EndpointSettings{"pods": {IPAddress: netip.MustParseAddr(ip), IPPrefixLen: 24}}}}
	}
	pod := func(stack, ip string) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, stack) }))
		t.Cleanup(srv.Close)
		member(stack, ip, map[string]int{"8080/tcp": srv.Listener.Addr().(*net.TCPAddr).Port})
	}
	pod("a", "10.244.0.5")
	pod("b", "10.244.0.6")
	member("c1", "10.244.0.21", nil)
	member("c2", "10.244.0.22", nil)
	epA, epB := netip.MustParseAddrPort("10.244.0.5:8080"), netip.MustParseAddrPort("10.244.0.6:8080")
	svc := Service{Name: "web", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.10"), SessionAffinity: "ClientIP",
		Ports: []ServicePort{{Port: 80, Endpoints: []netip.AddrPort{epA, epB}}}}
	if err := e.SetServices("pods", []Service{svc}); err != nil {
		t.Fatal(err)
	}
	portOf := func(stack string) int {
		path, _ := e.networkFile(stack)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m, ok := stackmembers.Find(stackmembers.Parse(b), "web")
		if !ok || m.Ports["80/tcp"] == 0 {
			t.Fatalf("%s's line for the service: %+v, %v", stack, m, ok)
		}
		return m.Ports["80/tcp"]
	}
	sl := e.svcs.listeners[serviceKey{"pods", "default", "web", 80, false}]
	p1, p2 := portOf("c1"), portOf("c2")
	if p1 == p2 || p1 == sl.port() {
		t.Fatalf("the stacks share a listener: %d, %d, the service's %d", p1, p2, sl.port())
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	get := func(port int) string {
		res, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
		if err != nil {
			return err.Error()
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	// The first connections go by turns, and each client keeps its own.
	first1, first2 := get(p1), get(p2)
	if first1 == first2 {
		t.Fatalf("both clients reached %q first", first1)
	}
	for range 3 {
		if g1, g2 := get(p1), get(p2); g1 != first1 || g2 != first2 {
			t.Errorf("the clients moved from %s, %s to %s, %s", first1, first2, g1, g2)
		}
	}

	// A client whose endpoint is gone takes another and keeps it.
	other := map[string]netip.AddrPort{"a": epB, "b": epA}[first1]
	svc.Ports[0].Endpoints = []netip.AddrPort{other}
	if err := e.SetServices("pods", []Service{svc}); err != nil {
		t.Fatal(err)
	}
	moved := get(p1)
	if moved == first1 {
		t.Fatalf("c1 still reached %s, which the service no longer has", first1)
	}
	svc.Ports[0].Endpoints = []netip.AddrPort{epA, epB}
	if err := e.SetServices("pods", []Service{svc}); err != nil {
		t.Fatal(err)
	}
	if got := get(p1); got != moved {
		t.Errorf("c1 moved back from %s to %s", moved, got)
	}
	if portOf("c1") != p1 {
		t.Error("an updated service moved the client's listener")
	}

	// Past the timeout, the client goes by turns again.
	sl.mu.Lock()
	c1 := netip.MustParseAddr("10.244.0.21")
	sl.sticky[c1] = stickyEndpoint{sl.sticky[c1].ep, time.Now().Add(-4 * time.Hour)}
	turn := map[string]int{"a": 1, "b": 0}[moved]
	sl.n = turn
	sl.mu.Unlock()
	if got := get(p1); got == moved {
		t.Errorf("past the timeout, c1 kept %s", got)
	}

	// The stack's listener goes with its last member.
	e.mu.Lock()
	delete(e.containers, "c2")
	e.mu.Unlock()
	if err := e.rewriteNetworks(); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(p2)); err == nil {
		c.Close()
		t.Error("a gone stack's listener still listens")
	}

	// Without affinity, the network files name the service's listener
	// again.
	svc.SessionAffinity = "None"
	if err := e.SetServices("pods", []Service{svc}); err != nil {
		t.Fatal(err)
	}
	if got := portOf("c1"); got != sl.port() {
		t.Errorf("without affinity, c1 reaches port %d, not the service's %d", got, sl.port())
	}
	// A router that reached c1's listener before still reaches the
	// service there, by turns.
	if got := get(p1) + get(p1); got != "ab" && got != "ba" {
		t.Errorf("c1's listener, without affinity: %q", got)
	}
	if err := e.SetServices("pods", nil); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(p1)); err == nil {
		c.Close()
		t.Error("a client's listener outlives the service")
	}
}

// A service answers at its external IPs as at its cluster IP, and at an
// IPv6 cluster IP beside its IPv4 one, which its names resolve to first;
// an IPv6-only service takes a member address of the member block's top,
// and an ExternalName service's names are aliases of its external name.
func TestServiceAddresses(t *testing.T) {
	a, err := alloc.Open(alloc.Config{Root: t.TempDir(), PortBase: 30000, PortLimit: 31000, BlockSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{alloc: a, containers: map[string]*Container{}, networks: map[string]*Network{
		"n1": {ID: "n1", Name: "pods", Driver: "bridge", Pool: Pool{Subnet: "10.244.0.0/24", Gateway: "10.244.0.1"}},
	}}
	defer e.closeServices()
	addr, err := e.assignAddressLocked("pod")
	if err != nil {
		t.Fatal(err)
	}
	e.containers["pod"] = &Container{rec: record{ID: "pod", Name: "pod", Stack: "pod", Address: addr,
		State:    stateRec{Status: container.StateRunning},
		Networks: map[string]*network.EndpointSettings{"pods": {IPAddress: netip.MustParseAddr("10.244.0.5"), IPPrefixLen: 24}}}}
	ep := []netip.AddrPort{netip.MustParseAddrPort("10.244.0.5:8080")}
	svcs := []Service{
		{Name: "dual", Namespace: "default", ClusterIP: netip.MustParseAddr("fd00:96::10"),
			ClusterIPs:  []netip.Addr{netip.MustParseAddr("fd00:96::10"), netip.MustParseAddr("10.96.0.10")},
			ExternalIPs: []netip.Addr{netip.MustParseAddr("192.0.2.7"), netip.MustParseAddr("2001:db8::7")},
			Ports:       []ServicePort{{Port: 80, Endpoints: ep}}},
		{Name: "six", Namespace: "default", ClusterIP: netip.MustParseAddr("fd00:96::11"), Ports: []ServicePort{{Port: 80, Endpoints: ep}}},
		{Name: "ext", Namespace: "default", ExternalName: "db.example.com"},
	}
	if err := e.SetServices("pods", svcs); err != nil {
		t.Fatal(err)
	}
	path, _ := e.networkFile("pod")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ms := stackmembers.Parse(b)
	dual, ok := stackmembers.Find(ms, "dual")
	if !ok || dual.Address != "10.96.0.10" || dual.On("pods") != "10.96.0.10" || dual.On6("pods") != "fd00:96::10" {
		t.Fatalf("the dual-stack service's line: %+v, %v", dual, ok)
	}
	for _, a := range []string{"10.96.0.10", "fd00:96::10", "192.0.2.7", "2001:db8::7"} {
		if m, ok := stackmembers.Find(ms, a); !ok || m.Address != dual.Address {
			t.Errorf("%s is not the dual-stack service's: %+v, %v", a, m, ok)
		}
	}
	six, ok := stackmembers.Find(ms, "six.default")
	if !ok || six.Address != "127.88.255.254" || six.On("pods") != "" || six.On6("pods") != "fd00:96::11" || six.Ports["80/tcp"] == 0 {
		t.Fatalf("the IPv6-only service's line: %+v, %v", six, ok)
	}
	if ext, ok := stackmembers.Find(ms, "ext.default.svc.cluster.local"); !ok || ext.Alias != "db.example.com" || ext.Address != "" || len(ext.Ports) != 0 {
		t.Errorf("the ExternalName service's line: %+v, %v", ext, ok)
	}
	if got := stackmembers.Unalias(ms, addr, "ext"); got != "db.example.com" {
		t.Errorf("the ExternalName service's name stands for %q", got)
	}
	// An update keeps the IPv6-only service's member address.
	if err := e.SetServices("pods", svcs[1:]); err != nil {
		t.Fatal(err)
	}
	if got := e.svcs.member[serviceID{"pods", "default", "six"}]; got != six.Address {
		t.Errorf("an update moved the IPv6-only service from %s to %s", six.Address, got)
	}
}

// A nested engine's UDP node port without a host port is the port's
// datagram socket in the privileged container's port directory, whose
// datagrams carry the client's address before their payload either way.
func TestUDPNodePortInPortDirectory(t *testing.T) {
	dir, err := os.MkdirTemp("", "ports")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	e := &Engine{cfg: Config{Nested: &Nested{Port: func(proto string, p int) (int, string, error) {
		if proto != "udp" {
			t.Errorf("asked for a %s port", proto)
		}
		return 0, dir, nil
	}}}}
	pc, err := e.listenUDPOnNode(30053)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	rt, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: router.UDPRouterPath(dir), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := rt.WriteToUnix([]byte{172, 18, 0, 3, 0x30, 0x39, 'h', 'i'}, &net.UnixAddr{Name: router.UDPSocketPath(dir, 30053), Net: "unixgram"}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, from, err := pc.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "hi" || from.String() != "172.18.0.3:12345" {
		t.Fatalf("read %q from %v: %v", buf[:n], from, err)
	}
	if _, err := pc.WriteTo([]byte("yo"), from); err != nil {
		t.Fatal(err)
	}
	rt.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err = rt.ReadFromUnix(buf)
	if err != nil || string(buf[:n]) != string([]byte{172, 18, 0, 3, 0x30, 0x39, 'y', 'o'}) {
		t.Fatalf("the router got %v: %v", buf[:n], err)
	}
}
