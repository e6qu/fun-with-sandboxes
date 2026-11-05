package router

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/members"
)

func listenFlow() (*net.UDPConn, error) {
	return net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
}

func portDir(t *testing.T) string {
	t.Helper()
	// A Unix socket's path is capped at 104 bytes on macOS.
	dir, err := os.MkdirTemp("/tmp", "fwsr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestJoinsToTheListener(t *testing.T) {
	dir := portDir(t)
	l, err := net.Listen("unix", SocketPath(dir, 7000))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		c.Write([]byte("echo " + line))
	}()
	r, err := Listen("127.0.0.1:0", dir, listenFlow, Network{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c, err := Dial(r.Addr().String(), 7000)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hello\n"))
	if got, _ := bufio.NewReader(c).ReadString('\n'); got != "echo hello\n" {
		t.Errorf("got %q", got)
	}
	if _, err := Dial(r.Addr().String(), 7001); err == nil {
		t.Error("a port nothing listens on was reached")
	}
}

// A client looks up a UDP port's flow port and exchanges datagrams with it;
// the container's socket sees each with its source, and its answers leave
// from the flow port.
func TestRelaysDatagrams(t *testing.T) {
	dir := portDir(t)
	r, err := Listen("127.0.0.1:0", dir, listenFlow, Network{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	server, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: UDPSocketPath(dir, 5353), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	flow, err := LookupUDP(r.Addr().String(), 5353)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := LookupUDP(r.Addr().String(), 5353); err != nil || again != flow {
		t.Fatalf("a second lookup gave %d (%v), want the flow port %d again", again, err, flow)
	}
	client, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: flow})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}
	server.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 128)
	n, _, err := server.ReadFromUnix(buf)
	if err != nil {
		t.Fatal(err)
	}
	src := netip.AddrPortFrom(netip.AddrFrom4([4]byte(buf[:4])), binary.BigEndian.Uint16(buf[4:]))
	if string(buf[Header:n]) != "query" || src.String() != client.LocalAddr().String() {
		t.Fatalf("the container's socket got %q from %s, want \"query\" from %s", buf[Header:n], src, client.LocalAddr())
	}
	// The answer goes back to the source the header names.
	answer := append(append([]byte{}, buf[:Header]...), "answer"...)
	if _, err := server.WriteToUnix(answer, &net.UnixAddr{Name: UDPRouterPath(dir), Net: "unixgram"}); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, from, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "answer" || from.Port != flow {
		t.Fatalf("the client got %q from %s, want \"answer\" from port %s", buf[:n], from, strconv.Itoa(flow))
	}
}

// dialForeign reaches port of another stack's member at addr through the
// router at router, as the container library does.
func dialForeign(router string, addr netip.Addr, port int) (net.Conn, error) {
	c, err := net.Dial("tcp", router)
	if err != nil {
		return nil, err
	}
	req := make([]byte, 8)
	binary.BigEndian.PutUint16(req, Foreign)
	v4 := addr.As4()
	copy(req[2:], v4[:])
	binary.BigEndian.PutUint16(req[6:], uint16(port))
	var ans [1]byte
	if _, err := c.Write(req); err == nil {
		_, err = io.ReadFull(c, ans[:])
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	if ans[0] != Connected {
		c.Close()
		return nil, errRefused
	}
	return c, nil
}

func echo(t *testing.T, l net.Listener) {
	t.Helper()
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				c.Write([]byte("echo " + line))
			}()
		}
	}()
}

// A router relays its container's connections to another stack's members
// on a network they share, at a host port or through the member's router,
// and refuses members on no network its container is on.
func TestRelaysToForeignMembers(t *testing.T) {
	hosted, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echo(t, hosted)
	theirDir := portDir(t)
	unhosted, err := net.Listen("unix", SocketPath(theirDir, 9000))
	if err != nil {
		t.Fatal(err)
	}
	echo(t, unhosted)
	theirs, err := Listen("127.0.0.1:0", theirDir, listenFlow, Network{})
	if err != nil {
		t.Fatal(err)
	}
	defer theirs.Close()

	file := filepath.Join(portDir(t), "network")
	ms := []members.Member{
		{Address: "127.88.0.2", Names: []string{"app"}, Networks: []string{"shared", "own"}},
		{Address: "127.88.0.3", Names: []string{"api"}, Ports: map[string]int{"8000/tcp": hosted.Addr().(*net.TCPAddr).Port}, Router: theirs.Addr().Port, Networks: []string{"shared"}, Foreign: true},
		{Address: "127.88.0.4", Names: []string{"db"}, Ports: map[string]int{"5432/tcp": hosted.Addr().(*net.TCPAddr).Port}, Networks: []string{"theirs"}, Foreign: true},
		// A container the router's links to, on no network they share.
		{Address: "127.88.0.5", Names: []string{"cache"}, Ports: map[string]int{"6379/tcp": hosted.Addr().(*net.TCPAddr).Port}, Foreign: true, Links: []members.Link{{Viewer: "127.88.0.2", Alias: "redis"}}},
	}
	if err := os.WriteFile(file, members.Format(ms), 0o644); err != nil {
		t.Fatal(err)
	}
	mine, err := Listen("127.0.0.1:0", portDir(t), listenFlow, Network{File: file, Address: "127.88.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	defer mine.Close()

	for _, port := range []int{8000, 9000} {
		c, err := dialForeign(mine.Addr().String(), netip.MustParseAddr("127.88.0.3"), port)
		if err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
		c.Write([]byte("hi\n"))
		if got, _ := bufio.NewReader(c).ReadString('\n'); got != "echo hi\n" {
			t.Errorf("port %d carried %q", port, got)
		}
		c.Close()
	}
	if c, err := dialForeign(mine.Addr().String(), netip.MustParseAddr("127.88.0.3"), 9001); err == nil {
		c.Close()
		t.Error("a port nothing listens on was reached")
	}
	if c, err := dialForeign(mine.Addr().String(), netip.MustParseAddr("127.88.0.4"), 5432); err == nil {
		c.Close()
		t.Error("a member on no shared network was reached")
	}
	if c, err := dialForeign(mine.Addr().String(), netip.MustParseAddr("127.88.0.5"), 6379); err != nil {
		t.Errorf("a linked member: %v", err)
	} else {
		c.Close()
	}
	if c, err := dialForeign(theirs.Addr().String(), netip.MustParseAddr("127.88.0.3"), 8000); err == nil {
		c.Close()
		t.Error("a router that knows no network relayed")
	}
}

// lookupForeign asks the router at router for its relay to UDP port port of
// another stack's member at addr, as the container library does.
func lookupForeign(router string, addr netip.Addr, port int) (int, error) {
	c, err := net.Dial("tcp", router)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	req := make([]byte, 10)
	binary.BigEndian.PutUint16(req[2:], Foreign)
	v4 := addr.As4()
	copy(req[4:], v4[:])
	binary.BigEndian.PutUint16(req[8:], uint16(port))
	if _, err := c.Write(req); err != nil {
		return 0, err
	}
	var ans [3]byte
	if _, err := io.ReadFull(c, ans[:1]); err != nil {
		return 0, err
	}
	if ans[0] != Connected {
		return 0, errRefused
	}
	if _, err := io.ReadFull(c, ans[1:]); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(ans[1:])), nil
}

// A router relays its container's datagrams to another stack's members'
// UDP ports, with a host port or without, from the member's stack's block,
// and their replies back.
func TestRelaysDatagramsToForeignMembers(t *testing.T) {
	hosted, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer hosted.Close()
	go func() {
		buf := make([]byte, 128)
		for {
			n, from, err := hosted.ReadFromUDP(buf)
			if err != nil {
				return
			}
			hosted.WriteToUDP(append([]byte("hosted "), buf[:n]...), from)
		}
	}()
	theirDir := portDir(t)
	unhosted, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: UDPSocketPath(theirDir, 5353), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer unhosted.Close()
	go func() {
		buf := make([]byte, 128)
		for {
			n, _, err := unhosted.ReadFromUnix(buf)
			if err != nil {
				return
			}
			if n < Header {
				continue
			}
			reply := append(append([]byte{}, buf[:Header]...), "unhosted "...)
			unhosted.WriteToUnix(append(reply, buf[Header:n]...), &net.UnixAddr{Name: UDPRouterPath(theirDir), Net: "unixgram"})
		}
	}()
	theirs, err := Listen("127.0.0.1:0", theirDir, listenFlow, Network{})
	if err != nil {
		t.Fatal(err)
	}
	defer theirs.Close()

	file := filepath.Join(portDir(t), "network")
	ms := []members.Member{
		{Address: "127.88.0.2", Names: []string{"app"}, Networks: []string{"shared"}},
		{Address: "127.88.0.3", Names: []string{"dns"}, Ports: map[string]int{"53/udp": hosted.LocalAddr().(*net.UDPAddr).Port}, Router: theirs.Addr().Port, Networks: []string{"shared"}, Foreign: true, Stack: "theirs"},
		{Address: "127.88.0.4", Names: []string{"db"}, Ports: map[string]int{"53/udp": hosted.LocalAddr().(*net.UDPAddr).Port}, Networks: []string{"theirs"}, Foreign: true, Stack: "theirs"},
	}
	if err := os.WriteFile(file, members.Format(ms), 0o644); err != nil {
		t.Fatal(err)
	}
	var smu sync.Mutex
	var stacks []string
	mine, err := Listen("127.0.0.1:0", portDir(t), listenFlow, Network{File: file, Address: "127.88.0.2",
		DialFlow: func(stack string, raddr *net.UDPAddr) (net.Conn, error) {
			smu.Lock()
			defer smu.Unlock()
			stacks = append(stacks, stack)
			return net.DialUDP("udp", nil, raddr)
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer mine.Close()

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for port, want := range map[int]string{53: "hosted hi", 5353: "unhosted hi"} {
		f, err := lookupForeign(mine.Addr().String(), netip.MustParseAddr("127.88.0.3"), port)
		if err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
		if again, err := lookupForeign(mine.Addr().String(), netip.MustParseAddr("127.88.0.3"), port); err != nil || again != f {
			t.Errorf("a second lookup of port %d: %d, %v, want %d", port, again, err, f)
		}
		relay := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: f}
		if _, err := client.WriteToUDP([]byte("hi"), relay); err != nil {
			t.Fatal(err)
		}
		client.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 128)
		n, from, err := client.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
		if string(buf[:n]) != want || from.Port != f {
			t.Errorf("port %d answered %q from %v, want %q from the relay", port, buf[:n], from, want)
		}
	}
	smu.Lock()
	if len(stacks) != 2 || stacks[0] != "theirs" || stacks[1] != "theirs" {
		t.Errorf("the relays sent from the blocks of %q", stacks)
	}
	smu.Unlock()
	if _, err := lookupForeign(mine.Addr().String(), netip.MustParseAddr("127.88.0.4"), 53); err == nil {
		t.Error("a member on no shared network was relayed to")
	}
}
