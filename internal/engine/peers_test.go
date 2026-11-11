package engine

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/alloc"
	stackmembers "github.com/e6qu/fun-with-sandboxes/internal/members"
)

// The pods of engines nested in containers on one network reach one
// another: each node's network files list the others' pods, and the
// outer engine dials them for a node's services. A node on another
// network sees none of them.
func TestNodesSeeOneAnothersPods(t *testing.T) {
	newAlloc := func(base int) *alloc.Allocator {
		a, err := alloc.Open(alloc.Config{Root: t.TempDir(), PortBase: base, PortLimit: base + 1000, BlockSize: 100})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	outer := &Engine{alloc: newAlloc(30000), containers: map[string]*Container{}, networks: map[string]*Network{}, nested: map[string]*nestedEngine{}}
	node := func(id, netName, subnet, podIP string, base int) (*Container, *Engine, string) {
		c := &Container{rec: record{ID: id, Name: id, Stack: id, State: stateRec{Status: container.StateRunning},
			Networks: map[string]*network.EndpointSettings{netName: {}}}}
		outer.containers[id] = c
		sub := &Engine{alloc: newAlloc(base), containers: map[string]*Container{}, networks: map[string]*Network{
			"p": {ID: "p", Name: "pods", Driver: "bridge", Pool: Pool{Subnet: subnet}},
		}}
		sub.cfg.Nested = &Nested{
			Publish: func(ms []stackmembers.Member) { outer.publishPeer(c, ms) },
			Peers:   func() []stackmembers.Member { return outer.peersOf(c.rec.ID) },
		}
		outer.nested[id] = &nestedEngine{e: sub}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, id) }))
		t.Cleanup(srv.Close)
		addr, err := sub.assignAddressLocked("pod")
		if err != nil {
			t.Fatal(err)
		}
		sub.containers["pod"] = &Container{rec: record{ID: "pod", Name: "pod", Stack: "pod", Address: addr,
			Ports: map[string]int{"80/tcp": srv.Listener.Addr().(*net.TCPAddr).Port}, State: stateRec{Status: container.StateRunning},
			Networks: map[string]*network.EndpointSettings{"pods": {IPAddress: netip.MustParseAddr(podIP), IPPrefixLen: 24}}}}
		return c, sub, podIP
	}
	_, subA, ipA := node("node-a-000000000", "kind", "10.244.0.0/24", "10.244.0.5", 31000)
	nodeB, subB, _ := node("node-b-000000000", "kind", "10.244.1.0/24", "10.244.1.5", 32000)
	_, subC, _ := node("node-c-000000000", "other", "10.244.2.0/24", "10.244.2.5", 33000)
	outer.refreshPeers("")
	for _, s := range []*Engine{subA, subB, subC} {
		s.mu.Lock()
		if err := s.writeNetworkLocked("pod"); err != nil {
			t.Fatal(err)
		}
		s.mu.Unlock()
	}
	read := func(s *Engine) []stackmembers.Member {
		path, _ := s.networkFile("pod")
		b, _ := os.ReadFile(path)
		return stackmembers.Parse(b)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		m, ok := stackmembers.Find(read(subB), ipA)
		if ok && m.Foreign && m.Ports["80/tcp"] != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node B's file never listed node A's pod: %+v", read(subB))
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if _, ok := stackmembers.Find(read(subC), ipA); ok {
		t.Error("a node on another network sees node A's pod")
	}
	c, err := outer.dialFrom(nodeB, netip.MustParseAddrPort(ipA+":80"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET / HTTP/1.0\r\n\r\n")
	b, _ := io.ReadAll(c)
	if want := "node-a-000000000"; len(b) < len(want) || string(b[len(b)-len(want):]) != want {
		t.Errorf("node B dialling node A's pod: %q", b)
	}
}
