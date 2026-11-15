package shim

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// A UDP port published in a port directory answers the clients the router
// names, each from a flow of its own, through the router's socket.
func TestListenUDPDir(t *testing.T) {
	dir, err := os.MkdirTemp("", "udpdir")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	var mu sync.Mutex
	sources := map[string]bool{}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			mu.Lock()
			sources[from.String()] = true
			mu.Unlock()
			echo.WriteTo(append([]byte("echo:"), buf[:n]...), from)
		}
	}()
	rt, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: router.UDPRouterPath(dir), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	// A port a listener holds already is skipped.
	held, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: router.UDPSocketPath(dir, 5353), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	cl, port, err := listenUDPDir(dir, 5353, 5354, echo.LocalAddr().String(), func(raddr *net.UDPAddr) (net.Conn, error) {
		return net.DialUDP("udp", nil, raddr)
	})
	if err != nil {
		t.Fatal(err)
	}
	if port != 5354 {
		t.Fatalf("bound port %d, want 5354", port)
	}
	sock := &net.UnixAddr{Name: router.UDPSocketPath(dir, port), Net: "unixgram"}
	for i, client := range []uint16{4001, 4002, 4001} {
		msg := binary.BigEndian.AppendUint16([]byte{127, 0, 0, 1}, client)
		msg = append(msg, "ping"...)
		if _, err := rt.WriteToUnix(msg, sock); err != nil {
			t.Fatal(err)
		}
		rt.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 1500)
		n, from, err := rt.ReadFromUnix(buf)
		if err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
		if from == nil || filepath.Base(from.Name) != filepath.Base(sock.Name) {
			t.Fatalf("reply %d came from %v, want the port's socket", i, from)
		}
		want := append(binary.BigEndian.AppendUint16([]byte{127, 0, 0, 1}, client), "echo:ping"...)
		if !bytes.Equal(buf[:n], want) {
			t.Fatalf("reply %d: %q, want %q", i, buf[:n], want)
		}
	}
	// A datagram too short for a header is dropped.
	if _, err := rt.WriteToUnix([]byte{1, 2}, sock); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	flows := len(sources)
	mu.Unlock()
	if flows != 2 {
		t.Fatalf("upstream saw %d flows, want one for each client", flows)
	}
	if err := cl.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock.Name); !os.IsNotExist(err) {
		t.Fatalf("the port's socket outlived its close: %v", err)
	}
}
