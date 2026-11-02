package egress

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"testing"

	"golang.org/x/net/proxy"
)

// echoServer answers each connection by echoing what it reads.
func echoServer(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// A SOCKS5 client reaches a destination the policy allows, by name or by
// address, on the port the HTTP side serves.
func TestSOCKS5ConnectsAllowedDestinations(t *testing.T) {
	p := startProxy(t)
	port := strconv.Itoa(echoServer(t))
	d, err := proxy.SOCKS5("tcp", p.Addr().String(), nil, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"remote.test:" + port, "127.0.0.1:" + port} {
		c, err := d.Dial("tcp", target)
		if err != nil {
			t.Fatalf("dial %s through SOCKS5: %v", target, err)
		}
		io.WriteString(c, "ping\n")
		line, err := bufio.NewReader(c).ReadString('\n')
		c.Close()
		if err != nil || line != "ping\n" {
			t.Fatalf("%s echoed %q, %v", target, line, err)
		}
	}
}

// socksRequest sends a raw SOCKS5 greeting with methods, then, when the
// proxy takes no-auth, a request, and returns the proxy's reply code.
func socksRequest(t *testing.T, p *Proxy, methods []byte, request []byte) byte {
	t.Helper()
	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(append([]byte{socksVersion, byte(len(methods))}, methods...))
	var choice [2]byte
	if _, err := io.ReadFull(c, choice[:]); err != nil {
		t.Fatal(err)
	}
	if choice[1] != socksNoAuth {
		return choice[1]
	}
	c.Write(request)
	var reply [4]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		t.Fatal(err)
	}
	return reply[1]
}

func connectRequest(cmd byte, host string, port uint16) []byte {
	req := []byte{socksVersion, cmd, 0x00, socksDomain, byte(len(host))}
	req = append(req, host...)
	return append(req, byte(port>>8), byte(port))
}

// The SOCKS5 side refuses what the HTTP side refuses, and what would open
// the host to a container.
func TestSOCKS5Refusals(t *testing.T) {
	p := startProxy(t)
	port := uint16(echoServer(t))
	for name, c := range map[string]struct {
		methods, request []byte
		want             byte
	}{
		"a host address":         {[]byte{socksNoAuth}, connectRequest(socksConnect, "rebind.test", port), socksNotAllowed},
		"an unknown name":        {[]byte{socksNoAuth}, connectRequest(socksConnect, "missing.test", port), socksHostUnreach},
		"BIND":                   {[]byte{socksNoAuth}, connectRequest(0x02, "remote.test", port), socksCmdUnsupport},
		"UDP ASSOCIATE":          {[]byte{socksNoAuth}, connectRequest(0x03, "remote.test", port), socksCmdUnsupport},
		"username/password only": {[]byte{0x02}, nil, socksNoMethod},
	} {
		if got := socksRequest(t, p, c.methods, c.request); got != c.want {
			t.Errorf("%s: reply %#x, want %#x", name, got, c.want)
		}
	}
}
