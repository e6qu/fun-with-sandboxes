package alloc

import (
	"errors"
	"net"
	"os"
	"strconv"
	"testing"
)

func open(t *testing.T, root string) *Allocator {
	t.Helper()
	a, err := Open(Config{Root: root, PortBase: 41000, PortLimit: 41040, BlockSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestStacksGetDisjointBlocks(t *testing.T) {
	a := open(t, t.TempDir())
	x, err := a.Stack("alpha")
	if err != nil {
		t.Fatal(err)
	}
	y, err := a.Stack("beta")
	if err != nil {
		t.Fatal(err)
	}
	if x.PortBase == y.PortBase {
		t.Fatalf("stacks share block %d", x.PortBase)
	}
	px, _ := a.Port("alpha", "db/5432")
	py, _ := a.Port("beta", "db/5432")
	if px == py {
		t.Fatalf("same service key in two stacks got the same port %d", px)
	}
	if !x.Contains(px) || x.Contains(py) || !y.Contains(py) {
		t.Fatalf("ports %d/%d escaped their blocks %d/%d", px, py, x.PortBase, y.PortBase)
	}
}

func TestPortIsStableAndDistinctWithinStack(t *testing.T) {
	a := open(t, t.TempDir())
	p1, _ := a.Port("alpha", "db/5432")
	p2, _ := a.Port("alpha", "cache/6379")
	again, _ := a.Port("alpha", "db/5432")
	if p1 == p2 {
		t.Fatalf("two services share port %d", p1)
	}
	if again != p1 {
		t.Fatalf("db port moved from %d to %d", p1, again)
	}
}

func TestPortSkipsHostPortsInUse(t *testing.T) {
	a := open(t, t.TempDir())
	s, _ := a.Stack("alpha")
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(s.PortBase))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p, err := a.Port("alpha", "db/5432")
	if err != nil {
		t.Fatal(err)
	}
	if p == s.PortBase {
		t.Fatalf("allocator handed out port %d that the host already holds", p)
	}
}

func TestAssignmentsPersist(t *testing.T) {
	root := t.TempDir()
	p, _ := open(t, root).Port("alpha", "db/5432")
	got, _ := open(t, root).Port("alpha", "db/5432")
	if got != p {
		t.Fatalf("after reopen port %d became %d", p, got)
	}
}

func TestReleaseFreesBlockAndDirectory(t *testing.T) {
	a := open(t, t.TempDir())
	s, _ := a.Stack("alpha")
	dir, _ := a.Dir("alpha")
	if err := a.Release("alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stack dir survived release: %v", err)
	}
	n, _ := a.Stack("gamma")
	if n.PortBase != s.PortBase {
		t.Fatalf("released block %d was not reused (got %d)", s.PortBase, n.PortBase)
	}
}

func TestRangeExhaustion(t *testing.T) {
	a := open(t, t.TempDir())
	for _, n := range []string{"a", "b", "c", "d"} {
		if _, err := a.Stack(n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Stack("e"); !errors.Is(err, ErrNoBlock) {
		t.Fatalf("fifth stack: got %v, want ErrNoBlock", err)
	}
}

func TestRejectsUnsafeNames(t *testing.T) {
	a := open(t, t.TempDir())
	for _, n := range []string{"", "../x", "A", "a/b", ".hidden"} {
		if _, err := a.Stack(n); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Stack(%q): got %v, want ErrInvalidName", n, err)
		}
	}
}

// A flow binds a port of the stack's block that no key holds and no other
// socket holds, reading the block from the state file.
func TestDialFlow(t *testing.T) {
	root := t.TempDir()
	a, err := Open(Config{Root: root, PortBase: 44000, PortLimit: 44100, BlockSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	raddr := peer.LocalAddr().(*net.UDPAddr)
	if _, err := DialFlow(root, "s", raddr); err == nil {
		t.Fatal("dialled a flow for a stack that holds no block")
	}
	first, err := a.Port("s", "a/80/tcp")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := a.Stack("s")
	one, err := DialFlow(root, "s", raddr)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := DialFlow(root, "s", raddr)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	p1, p2 := one.LocalAddr().(*net.UDPAddr).Port, two.LocalAddr().(*net.UDPAddr).Port
	if p1 == first || p2 == first || p1 == p2 || !st.Contains(p1) || !st.Contains(p2) {
		t.Fatalf("flows bound %d and %d; want two distinct ports of the block other than %d", p1, p2, first)
	}
	next, err := a.Port("s", "b/80/tcp")
	if err != nil || next == p1 || next == p2 {
		t.Fatalf("a key took a flow's port: %d, %v", next, err)
	}
}
