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

// A lease takes a port of the stack's block that no key holds, and assigned
// keys skip it until it returns.
func TestLease(t *testing.T) {
	a, err := Open(Config{Root: t.TempDir(), PortBase: 44000, PortLimit: 44100, BlockSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Lease("s"); err == nil {
		t.Fatal("leased a port of a stack that holds no block")
	}
	first, err := a.Port("s", "a/80/tcp")
	if err != nil {
		t.Fatal(err)
	}
	l, err := a.Lease("s")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := a.Stack("s")
	if l == first || !st.Contains(l) {
		t.Fatalf("leased %d, want a free port of the block other than %d", l, first)
	}
	next, err := a.Port("s", "b/80/tcp")
	if err != nil || next == l {
		t.Fatalf("a key took the leased port %d: %d, %v", l, next, err)
	}
	a.Unlease("s", l)
	if again, err := a.Lease("s"); err != nil || again != l {
		t.Fatalf("leasing after the return got %d, %v; want %d", again, err, l)
	}
}
