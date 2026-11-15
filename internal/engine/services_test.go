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
		"an IPv6 IP":   {Name: "web", Namespace: "default", ClusterIP: netip.MustParseAddr("fd00::1")},
		"an SCTP port": {Name: "dns", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.12"), Ports: []ServicePort{{Port: 53, Protocol: "SCTP"}}},
		"a UDP node port": {Name: "dns", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.14"),
			Ports: []ServicePort{{Port: 53, Protocol: "UDP", NodePort: 30053}}},
		"a dotted name": {Name: "a.b", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.13")},
	} {
		if err := e.SetServices("pods", []Service{bad}); err == nil {
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
	ask := func() string {
		c, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(m.Ports["53/udp"]))
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
	if got := ask() + " " + ask(); got != "dns-a:q dns-b:q" && got != "dns-b:q dns-a:q" {
		t.Errorf("the endpoints answered %q, want them by turns", got)
	}
}
