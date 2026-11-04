package router

import (
	"bufio"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"
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
	r, err := Listen("127.0.0.1:0", dir, listenFlow)
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
	r, err := Listen("127.0.0.1:0", dir, listenFlow)
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
