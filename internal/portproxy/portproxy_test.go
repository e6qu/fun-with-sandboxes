package portproxy

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/alloc"
)

// udpEcho answers each datagram with its payload prefixed by tag.
func udpEcho(t *testing.T, tag string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte(tag), buf[:n]...), from)
		}
	}()
	return pc.LocalAddr().String()
}

func exchange(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

// The UDP proxy forwards datagrams to the container's port and each reply
// to the client that sent the request.
func TestUDPProxyKeepsAFlowPerClient(t *testing.T) {
	upstream := udpEcho(t, "echo:")
	root := t.TempDir()
	a0, err := alloc.Open(alloc.Config{Root: root, PortBase: 43000, PortLimit: 43100, BlockSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	block, err := a0.Stack("udp")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var leased []int
	dial := func(raddr *net.UDPAddr) (net.Conn, error) {
		c, err := alloc.DialFlow(root, "udp", raddr)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		leased = append(leased, c.LocalAddr().(*net.UDPAddr).Port)
		mu.Unlock()
		return c, nil
	}
	cl, port, err := Listen("udp", netip.MustParseAddr("127.0.0.1"), 0, 0, upstream, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	a, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := exchange(t, a, "from a"); got != "echo:from a" {
		t.Fatalf("client a got %q", got)
	}
	if got := exchange(t, b, "from b"); got != "echo:from b" {
		t.Fatalf("client b got %q", got)
	}
	if got := exchange(t, a, "again"); got != "echo:again" {
		t.Fatalf("client a's second exchange got %q", got)
	}
	// Each client's flow sends from its own port of the stack's block.
	mu.Lock()
	defer mu.Unlock()
	if len(leased) != 2 || leased[0] == leased[1] || !block.Contains(leased[0]) || !block.Contains(leased[1]) {
		t.Fatalf("flows sent from %v, want two ports of %d-%d", leased, block.PortBase, block.PortBase+block.BlockSize-1)
	}
	if err := cl.Close(); err != nil {
		t.Fatal(err)
	}
}

// A host port range binds its first port the host has free, and fails as
// a taken port does once every port of it is taken.
func TestListenRangeTakesTheFirstFreePort(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	lo := held.Addr().(*net.TCPAddr).Port
	ip := netip.MustParseAddr("127.0.0.1")
	cl, port, err := Listen("tcp", ip, lo, lo+5, "127.0.0.1:1", nil)
	if err != nil {
		t.Fatalf("binding %d-%d with %d taken: %v", lo, lo+5, lo, err)
	}
	defer cl.Close()
	if port <= lo || port > lo+5 {
		t.Fatalf("bound %d, want a port in %d-%d other than %d", port, lo+1, lo+5, lo)
	}
	if _, _, err := Listen("tcp", ip, lo, lo, "127.0.0.1:1", nil); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("binding a fully taken range: %v", err)
	}
}

func TestHostPortRange(t *testing.T) {
	for in, want := range map[string][2]int{"": {0, 0}, "8080": {8080, 8080}, "8000-8010": {8000, 8010}} {
		lo, hi, err := HostPortRange(in)
		if err != nil || lo != want[0] || hi != want[1] {
			t.Errorf("HostPortRange(%q) = %d, %d, %v", in, lo, hi, err)
		}
	}
	for _, bad := range []string{"x", "9-1", "70000"} {
		if _, _, err := HostPortRange(bad); err == nil {
			t.Errorf("HostPortRange(%q) accepted it", bad)
		}
	}
}
