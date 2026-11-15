package router

import (
	"net"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestParseResolvConf(t *testing.T) {
	d := ParseResolvConf([]byte("# comment\nnameserver 1.1.1.1\nnameserver 2606:4700::1111\nnameserver bogus\ndomain old.example\nsearch a.example b.example. .\noptions ndots:3 timeout:2 attempts:9 rotate\n"))
	want := DNS{
		Servers: []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2606:4700::1111")},
		Search:  []string{"a.example", "b.example"}, Ndots: 3, Timeout: 2 * time.Second, Attempts: 5,
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("got %+v, want %+v", d, want)
	}
	if d := ParseResolvConf(nil); d.Ndots != 1 || d.Timeout != 5*time.Second || d.Attempts != 2 || d.Servers != nil {
		t.Errorf("defaults: %+v", d)
	}
}

func TestCandidates(t *testing.T) {
	d := DNS{Search: []string{"a.example", "b.example"}, Ndots: 1}
	for name, want := range map[string][]string{
		"web":          {"web.a.example.", "web.b.example.", "web."},
		"www.test":     {"www.test.", "www.test.a.example.", "www.test.b.example."},
		"rooted.name.": {"rooted.name."},
	} {
		if got := d.candidates(name); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
	d.Ndots = 2
	if got := d.candidates("www.test"); got[0] != "www.test.a.example." || got[2] != "www.test." {
		t.Errorf("ndots:2: got %q", got)
	}
}

// fakeDNS answers on loopback from records, by lowercase absolute name; a
// name it lacks is NXDOMAIN. It records the names it was asked.
type fakeDNS struct {
	pc      net.PacketConn
	records map[string][]netip.Addr
	mu      sync.Mutex
	asked   []string
}

func serveDNS(t *testing.T, records map[string][]netip.Addr) *fakeDNS {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	f := &fakeDNS{pc: pc, records: records}
	go f.serve()
	return f
}

func (f *fakeDNS) port() int { return f.pc.LocalAddr().(*net.UDPAddr).Port }

func (f *fakeDNS) serve() {
	buf := make([]byte, 512)
	for {
		n, from, err := f.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		var m dnsmessage.Message
		if m.Unpack(buf[:n]) != nil || len(m.Questions) != 1 {
			continue
		}
		q := m.Questions[0]
		name := strings.ToLower(q.Name.String())
		f.mu.Lock()
		f.asked = append(f.asked, name+" "+q.Type.String())
		f.mu.Unlock()
		m.Response = true
		addrs, ok := f.records[name]
		if !ok {
			m.RCode = dnsmessage.RCodeNameError
		}
		for _, a := range addrs {
			h := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}
			switch {
			case a.Is4() && q.Type == dnsmessage.TypeA:
				h.Type = dnsmessage.TypeA
				m.Answers = append(m.Answers, dnsmessage.Resource{Header: h, Body: &dnsmessage.AResource{A: a.As4()}})
			case a.Is6() && q.Type == dnsmessage.TypeAAAA:
				h.Type = dnsmessage.TypeAAAA
				m.Answers = append(m.Answers, dnsmessage.Resource{Header: h, Body: &dnsmessage.AAAAResource{AAAA: a.As16()}})
			}
		}
		if b, err := m.Pack(); err == nil {
			f.pc.WriteTo(b, from)
		}
	}
}

// A container with DNS of its own resolves through its servers, applying
// its search domains, and only when it has internet access.
func TestResolveThroughTheContainersServers(t *testing.T) {
	v4, v6 := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")
	f := serveDNS(t, map[string][]netip.Addr{
		"web.corp.example.": {v4, v6},
		"www.test.":         {netip.MustParseAddr("192.0.2.20")},
	})
	// The first server, where nothing serves, fails or does not answer;
	// the second answers.
	dns := &DNS{Servers: []netip.Addr{netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.1")}, Search: []string{"corp.example"}, Ndots: 1, Timeout: 300 * time.Millisecond, Attempts: 1, port: f.port()}
	r, err := Listen("127.0.0.1:0", portDir(t), listenFlow, Network{Internet: true, DNS: dns})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := LookupName(r.Addr().String(), 0, "web")
	if err != nil || !reflect.DeepEqual(got, []netip.Addr{v4, v6}) {
		t.Errorf("web: %v, %v", got, err)
	}
	if got, err := LookupName(r.Addr().String(), 6, "web"); err != nil || !reflect.DeepEqual(got, []netip.Addr{v6}) {
		t.Errorf("web, IPv6: %v, %v", got, err)
	}
	if got, err := LookupName(r.Addr().String(), 4, "www.test"); err != nil || len(got) != 1 || got[0].String() != "192.0.2.20" {
		t.Errorf("www.test: %v, %v", got, err)
	}
	if _, err := LookupName(r.Addr().String(), 0, "missing"); err == nil {
		t.Error("a name the server does not know resolved")
	}
	f.mu.Lock()
	asked := strings.Join(f.asked, ",")
	f.mu.Unlock()
	if !strings.Contains(asked, "missing.corp.example. TypeA") || !strings.Contains(asked, "missing. TypeA") {
		t.Errorf("the search domain went unused: %s", asked)
	}

	closed, err := Listen("127.0.0.1:0", portDir(t), listenFlow, Network{DNS: dns})
	if err != nil {
		t.Fatal(err)
	}
	defer closed.Close()
	if _, err := LookupName(closed.Addr().String(), 0, "web"); err == nil {
		t.Error("a container without internet access resolved through its servers")
	}
}

func TestResolveAnswer(t *testing.T) {
	if got := resolveAnswer(nil); !reflect.DeepEqual(got, []byte{Refused}) {
		t.Errorf("no addresses: %v", got)
	}
	got := resolveAnswer([]netip.Addr{netip.MustParseAddr("::ffff:192.0.2.1"), netip.MustParseAddr("2001:db8::1")})
	want := append([]byte{Connected, 2, 4, 192, 0, 2, 1, 16}, netip.MustParseAddr("2001:db8::1").AsSlice()...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
