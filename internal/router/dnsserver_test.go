package router

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/e6qu/fun-with-sandboxes/internal/members"
)

// askDNS sends one question to the DNS server at addr and returns its
// answer.
func askDNS(t *testing.T, addr net.Addr, name string, typ dnsmessage.Type) dnsmessage.Message {
	t.Helper()
	q, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 4242, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}},
	}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(q); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 65535)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("%s %s: %v", name, typ, err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf[:n]); err != nil {
		t.Fatal(err)
	}
	if m.ID != 4242 || !m.Response {
		t.Fatalf("%s %s: answer %+v", name, typ, m.Header)
	}
	return m
}

func answerAddrs(m dnsmessage.Message) []string {
	var out []string
	for _, rr := range m.Answers {
		switch b := rr.Body.(type) {
		case *dnsmessage.AResource:
			out = append(out, netip.AddrFrom4(b.A).String())
		case *dnsmessage.AAAAResource:
			out = append(out, netip.AddrFrom16(b.AAAA).String())
		}
	}
	return out
}

// The router answers DNS at its own address: the names its container's
// stack answers, as the container sees them, itself, and any other name
// through the container's servers, given internet access.
func TestRouterServesDNS(t *testing.T) {
	file := filepath.Join(portDir(t), "network")
	ms := []members.Member{
		{Address: "127.88.0.2", Names: []string{"app"}, Networks: []string{"net"}},
		{Address: "127.88.0.3", Names: []string{"db", "db.example"}, Networks: []string{"net"},
			Subnets: []members.NetAddress{{Network: "net", Address: netip.MustParsePrefix("fd00::3/64")}}},
		{Address: "127.88.0.4", Names: []string{"elsewhere"}, Networks: []string{"other"}},
		{Alias: "db", Names: []string{"database"}, Networks: []string{"net"}},
	}
	if err := os.WriteFile(file, members.Format(ms), 0o644); err != nil {
		t.Fatal(err)
	}
	up := serveDNS(t, map[string][]netip.Addr{"www.test.": {netip.MustParseAddr("192.0.2.20")}})
	r, err := Listen("127.0.0.1:0", portDir(t), listenFlow, Network{File: file, Address: "127.88.0.2", Internet: true, Upstream: []netip.Addr{netip.MustParseAddr("127.0.0.1")}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.upstreamPort = up.port()

	for _, c := range []struct {
		name string
		typ  dnsmessage.Type
		want []string
	}{
		{"db.", dnsmessage.TypeA, []string{"127.88.0.3"}},
		{"DB.example.", dnsmessage.TypeA, []string{"127.88.0.3"}},
		{"db.", dnsmessage.TypeAAAA, []string{"fd00::3"}},
		// An alias answers as the name it stands for.
		{"database.", dnsmessage.TypeA, []string{"127.88.0.3"}},
		// A name the stack answers has no records of other types.
		{"db.", dnsmessage.TypeMX, nil},
	} {
		m := askDNS(t, r.dns.LocalAddr(), c.name, c.typ)
		if got := answerAddrs(m); m.RCode != dnsmessage.RCodeSuccess || !equalStrings(got, c.want) {
			t.Errorf("%s %s: %s %v, want %v", c.name, c.typ, m.RCode, got, c.want)
		}
	}
	// A member on no network the container is on is not the stack's to
	// answer: the question goes on to the container's servers.
	if m := askDNS(t, r.dns.LocalAddr(), "elsewhere.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeNameError {
		t.Errorf("a member the container does not see: %s %v", m.RCode, answerAddrs(m))
	}
	if m := askDNS(t, r.dns.LocalAddr(), "www.test.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeSuccess || !equalStrings(answerAddrs(m), []string{"192.0.2.20"}) {
		t.Errorf("www.test: %s %v", m.RCode, answerAddrs(m))
	}

	// Without internet access, the container's servers are out of reach:
	// the router refuses what its stack does not answer.
	closed, err := Listen("127.0.0.1:0", portDir(t), listenFlow, Network{File: file, Address: "127.88.0.2", Upstream: []netip.Addr{netip.MustParseAddr("127.0.0.1")}})
	if err != nil {
		t.Fatal(err)
	}
	defer closed.Close()
	closed.upstreamPort = up.port()
	if m := askDNS(t, closed.dns.LocalAddr(), "www.test.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeRefused {
		t.Errorf("www.test without internet access: %s", m.RCode)
	}
	if m := askDNS(t, closed.dns.LocalAddr(), "app.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeSuccess || !equalStrings(answerAddrs(m), []string{"127.88.0.2"}) {
		t.Errorf("app without internet access: %s %v", m.RCode, answerAddrs(m))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
