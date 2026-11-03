package router

import (
	"bufio"
	"net"
	"os"
	"testing"
)

func TestJoinsToTheListener(t *testing.T) {
	// A Unix socket's path is capped at 104 bytes on macOS.
	dir, err := os.MkdirTemp("/tmp", "fwsr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
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
	r, err := Listen("127.0.0.1:0", dir)
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
