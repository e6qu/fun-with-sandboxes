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
	if c, err := dialForeign(theirs.Addr().String(), netip.MustParseAddr("127.88.0.3"), 8000); err == nil {
		c.Close()
		t.Error("a router that knows no network relayed")
	}
}
