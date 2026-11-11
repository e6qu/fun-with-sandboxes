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
		"no namespace":  {Name: "web", ClusterIP: netip.MustParseAddr("10.96.0.11")},
		"an IPv6 IP":    {Name: "web", Namespace: "default", ClusterIP: netip.MustParseAddr("fd00::1")},
		"a UDP port":    {Name: "dns", Namespace: "default", ClusterIP: netip.MustParseAddr("10.96.0.12"), Ports: []ServicePort{{Port: 53, Protocol: "UDP"}}},
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
