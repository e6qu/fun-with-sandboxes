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
	clients := make(chan netip.AddrPort, 1)
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
		from, err := ReadClient(c)
		if err != nil {
			return
		}
		clients <- from
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
	if from, want := <-clients, c.LocalAddr().(*net.TCPAddr).AddrPort(); from != want {
		t.Errorf("the listener's client is %v, want %v", from, want)
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

// readKey reads the relay key of the router whose port directory is dir,
// as the container library does.
func readKey(t *testing.T, dir string) []byte {
	t.Helper()
	key, err := os.ReadFile(KeyPath(dir))
	if err != nil || len(key) != KeySize {
		t.Fatalf("the relay key: %x, %v", key, err)
	}
	return key
}

// foreignRequest is a Foreign request with key for port of the member at
// addr.
func foreignRequest(key []byte, addr netip.Addr, port int) []byte {
	v4 := addr.As4()
	req := binary.BigEndian.AppendUint16(nil, Foreign)
	req = append(append(req, key...), v4[:]...)
	return binary.BigEndian.AppendUint16(req, uint16(port))
}

// dialForeign reaches port of another stack's member at addr through the
// router at router, as the container library does.
func dialForeign(router string, key []byte, addr netip.Addr, port int) (net.Conn, error) {
	c, err := net.Dial("tcp", router)
	if err != nil {
		return nil, err
	}
	req := foreignRequest(key, addr, port)
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

// echo answers each line a client sends on l; on a container's listener,
// whose connections the router makes, after the client's address.
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
				if _, ok := l.(*net.UnixListener); ok {
					if _, err := ReadClient(c); err != nil {
						return
					}
				}
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
	myDir := portDir(t)
	mine, err := Listen("127.0.0.1:0", myDir, listenFlow, Network{File: file, Address: "127.88.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	defer mine.Close()
	key := readKey(t, myDir)

	for _, port := range []int{8000, 9000} {
		c, err := dialForeign(mine.Addr().String(), key, netip.MustParseAddr("127.88.0.3"), port)
		if err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
		c.Write([]byte("hi\n"))
		if got, _ := bufio.NewReader(c).ReadString('\n'); got != "echo hi\n" {
			t.Errorf("port %d carried %q", port, got)
		}
		c.Close()
	}
	if c, err := dialForeign(mine.Addr().String(), key, netip.MustParseAddr("127.88.0.3"), 9001); err == nil {
		c.Close()
		t.Error("a port nothing listens on was reached")
	}
	if c, err := dialForeign(mine.Addr().String(), key, netip.MustParseAddr("127.88.0.4"), 5432); err == nil {
		c.Close()
		t.Error("a member on no shared network was reached")
	}
	if c, err := dialForeign(mine.Addr().String(), key, netip.MustParseAddr("127.88.0.5"), 6379); err != nil {
		t.Errorf("a linked member: %v", err)
	} else {
		c.Close()
	}
	if c, err := dialForeign(theirs.Addr().String(), readKey(t, theirDir), netip.MustParseAddr("127.88.0.3"), 8000); err == nil {
		c.Close()
		t.Error("a router that knows no network relayed")
	}
	// Another member of the stack reaches the router but not its key.
	if c, err := dialForeign(mine.Addr().String(), make([]byte, KeySize), netip.MustParseAddr("127.88.0.3"), 8000); err == nil {
		c.Close()
		t.Error("a request without the relay key was relayed")
	}
	// A router restarted for the running container keeps its key.
	mine.Close()
	again, err := Listen("127.0.0.1:0", myDir, listenFlow, Network{File: file, Address: "127.88.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if c, err := dialForeign(again.Addr().String(), key, netip.MustParseAddr("127.88.0.3"), 8000); err != nil {
		t.Errorf("the restarted router: %v", err)
	} else {
		c.Close()
	}
}

// lookupForeign asks the router at router for its relay to UDP port port of
// another stack's member at addr, as the container library does.
func lookupForeign(router string, key []byte, addr netip.Addr, port int) (int, error) {
	c, err := net.Dial("tcp", router)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	req := append([]byte{0, 0}, foreignRequest(key, addr, port)...)
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
	var hmu sync.Mutex
	var heard []string
	go func() {
		buf := make([]byte, 128)
		for {
			n, from, err := hosted.ReadFromUDP(buf)
			if err != nil {
				return
			}
			hmu.Lock()
			heard = append(heard, string(buf[:n]))
			hmu.Unlock()
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

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	file := filepath.Join(portDir(t), "network")
	ms := []members.Member{
		{Address: "127.88.0.2", Names: []string{"app"}, Ports: map[string]int{"9000/udp": client.LocalAddr().(*net.UDPAddr).Port}, Networks: []string{"shared"}},
		{Address: "127.88.0.3", Names: []string{"dns"}, Ports: map[string]int{"53/udp": hosted.LocalAddr().(*net.UDPAddr).Port}, Router: theirs.Addr().Port, Networks: []string{"shared"}, Foreign: true, Stack: "theirs"},
		{Address: "127.88.0.4", Names: []string{"db"}, Ports: map[string]int{"53/udp": hosted.LocalAddr().(*net.UDPAddr).Port}, Networks: []string{"theirs"}, Foreign: true, Stack: "theirs"},
	}
	if err := os.WriteFile(file, members.Format(ms), 0o644); err != nil {
		t.Fatal(err)
	}
	var smu sync.Mutex
	var stacks []string
	myDir := portDir(t)
	mine, err := Listen("127.0.0.1:0", myDir, listenFlow, Network{File: file, Address: "127.88.0.2",
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
	key := readKey(t, myDir)

	for port, want := range map[int]string{53: "hosted hi", 5353: "unhosted hi"} {
		f, err := lookupForeign(mine.Addr().String(), key, netip.MustParseAddr("127.88.0.3"), port)
		if err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
		if again, err := lookupForeign(mine.Addr().String(), key, netip.MustParseAddr("127.88.0.3"), port); err != nil || again != f {
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
	f, err := lookupForeign(mine.Addr().String(), key, netip.MustParseAddr("127.88.0.3"), 53)
	if err != nil {
		t.Fatal(err)
	}
	relay := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(f))

	// A socket of the container on a port without a host port sends
	// through the router, from the port's flow port.
	own, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: UDPSocketPath(myDir, 7000), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	dgram := make([]byte, Header, Header+2)
	copy(dgram, relay.Addr().AsSlice())
	binary.BigEndian.PutUint16(dgram[4:], relay.Port())
	if _, err := own.WriteToUnix(append(dgram, "yo"...), &net.UnixAddr{Name: UDPRouterPath(myDir), Net: "unixgram"}); err != nil {
		t.Fatal(err)
	}
	own.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 128)
	if n, _, err := own.ReadFromUnix(buf); err != nil || string(buf[Header:n]) != "hosted yo" {
		t.Errorf("the relay answered the container's flow port with %q, %v", buf[:max(n, 0)], err)
	}

	// Another member of the stack reaches the relay's port, but the relay
	// takes no datagram from it.
	other, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.WriteToUDPAddrPort([]byte("intruder"), relay); err != nil {
		t.Fatal(err)
	}
	// The relay takes datagrams in order, so once the container's next one
	// is answered, the other member's was relayed already or never.
	if _, err := client.WriteToUDPAddrPort([]byte("after"), relay); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, _, err := client.ReadFromUDP(buf); err != nil || string(buf[:n]) != "hosted after" {
		t.Fatalf("the relay answered the container with %q, %v", buf[:max(n, 0)], err)
	}
	hmu.Lock()
	for _, h := range heard {
		if h == "intruder" {
			t.Error("the relay took a datagram from another member's socket")
		}
	}
	hmu.Unlock()

	smu.Lock()
	if len(stacks) != 3 || stacks[0] != "theirs" || stacks[1] != "theirs" || stacks[2] != "theirs" {
		t.Errorf("the relays sent from the blocks of %q", stacks)
	}
	smu.Unlock()
	if _, err := lookupForeign(mine.Addr().String(), key, netip.MustParseAddr("127.88.0.4"), 53); err == nil {
		t.Error("a member on no shared network was relayed to")
	}
	if _, err := lookupForeign(mine.Addr().String(), make([]byte, KeySize), netip.MustParseAddr("127.88.0.3"), 53); err == nil {
		t.Error("a lookup without the relay key was answered")
	}
}
