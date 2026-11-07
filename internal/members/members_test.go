package members

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestFormatParseFind(t *testing.T) {
	ms := []Member{
		{Address: "127.88.0.2", Names: []string{"proj-db-1", "db"}, Ports: map[string]int{"5432/tcp": 30010}, Router: 30013, Networks: []string{"n1"}},
		{Address: "127.88.0.3", Names: []string{"web"}, Ports: map[string]int{"80/tcp": 30011, "53/udp": 30012}, Router: 30014, Networks: []string{"n1", "n2"}, Foreign: true, Stack: "other", Links: []Link{{"127.88.0.9", "www"}, {"127.88.0.2", "front"}}},
	}
	b := Format(ms)
	if want := "127.88.0.2\tproj-db-1 db\t5432/tcp=30010\t30013\tn1\t0\t\t\n127.88.0.3\tweb\t53/udp=30012,80/tcp=30011\t30014\tn1 n2\t1\tother\t127.88.0.9/www 127.88.0.2/front\n"; string(b) != want {
		t.Fatalf("Format:\n%s\nwant:\n%s", b, want)
	}
	if got := Parse(append(b, "garbage\n"...)); !reflect.DeepEqual(got, ms) {
		t.Fatalf("Parse: %+v", got)
	}
	for host, want := range map[string]string{"DB.": "127.88.0.2", "web": "127.88.0.3", "127.88.0.3": "127.88.0.3", "Front": "127.88.0.3"} {
		if m, ok := Find(ms, host); !ok || m.Address != want {
			t.Errorf("Find(%q): %+v, %v", host, m, ok)
		}
	}
	if !Share(ms[0], ms[1]) || Share(ms[0], Member{Networks: []string{"n2"}}) {
		t.Error("Share misreads the members' networks")
	}
	// A link lets its viewer see a member it shares no network with.
	viewer := Member{Address: "127.88.0.9", Networks: []string{"bridge"}}
	if !Sees(viewer, ms[1]) || Sees(viewer, ms[0]) || !Sees(ms[0], ms[1]) {
		t.Error("Sees misreads the members' links")
	}
	// Files of six and seven fields still read.
	if got := Parse([]byte("127.88.0.2\tdb\t\t0\tn1\t0\n127.88.0.3\tweb\t\t0\tn1\t1\tother\n")); len(got) != 2 || got[1].Stack != "other" {
		t.Errorf("Parse of older lines: %+v", got)
	}
	if _, ok := Find(ms, "example.com"); ok {
		t.Error("a name no member answers to was found")
	}
}

// A member resolves the names of the members it sees alone, and the
// aliases its own links give them.
func TestLookupFromAViewer(t *testing.T) {
	ms := []Member{
		{Address: "127.88.0.2", Names: []string{"api"}, Networks: []string{"front"}},
		{Address: "127.88.0.3", Names: []string{"db"}, Networks: []string{"back"}, Links: []Link{{"127.88.0.4", "database"}}},
		{Address: "127.88.0.4", Names: []string{"worker"}, Networks: []string{"front"}},
		{Address: "127.88.0.5", Names: []string{"cache"}, Networks: []string{"front"}, Foreign: true, Stack: "other"},
	}
	for _, c := range []struct {
		self, host, want string
	}{
		{"127.88.0.2", "worker", "127.88.0.4"},
		{"127.88.0.2", "API.", "127.88.0.2"},
		{"127.88.0.2", "cache", "127.88.0.5"},
		{"127.88.0.2", "db", ""},
		{"127.88.0.2", "127.88.0.3", ""},
		{"127.88.0.2", "database", ""},
		{"127.88.0.4", "database", "127.88.0.3"},
		{"127.88.0.4", "db", "127.88.0.3"},
		{"127.88.0.3", "api", ""},
		{"127.88.0.9", "api", ""},
		{"", "api", ""},
		{"api", "api", ""},
	} {
		m, ok := Lookup(ms, c.self, c.host)
		if ok != (c.want != "") || m.Address != c.want {
			t.Errorf("Lookup from %s of %q: %q, %v; want %q", c.self, c.host, m.Address, ok, c.want)
		}
	}
}

// A member's addresses on networks with subnets of their own read back,
// name it, and are the ones a viewer on such a network reaches it at.
func TestSubnetAddresses(t *testing.T) {
	ms := []Member{
		{Address: "127.88.0.2", Names: []string{"api"}, Networks: []string{"back", "front"}, Subnets: []NetAddress{{"back", netip.MustParsePrefix("172.28.0.5/16")}}},
		{Address: "127.88.0.3", Names: []string{"db"}, Networks: []string{"back"}, Subnets: []NetAddress{{"back", netip.MustParsePrefix("172.28.0.6/16")}}},
		{Address: "127.88.0.4", Names: []string{"web"}, Networks: []string{"front"}},
	}
	b := Format(ms)
	if want := "127.88.0.2\tapi\t\t0\tback front\t0\t\t\tback=172.28.0.5/16\n"; !strings.HasPrefix(string(b), want) {
		t.Fatalf("Format:\n%s\nwant a first line of:\n%s", b, want)
	}
	if !strings.Contains(string(b), "127.88.0.4\tweb\t\t0\tfront\t0\t\t\n") {
		t.Errorf("a member on no subnet gets a ninth field:\n%s", b)
	}
	got := Parse(b)
	if !reflect.DeepEqual(got[0].Subnets, ms[0].Subnets) || len(got[2].Subnets) != 0 {
		t.Fatalf("Parse: %+v", got)
	}
	if m, ok := Find(got, "172.28.0.6"); !ok || m.Address != "127.88.0.3" {
		t.Errorf("Find by a subnet address: %+v, %v", m, ok)
	}
	if m, ok := Lookup(got, "127.88.0.3", "172.28.0.5"); !ok || m.Address != "127.88.0.2" {
		t.Errorf("Lookup by a subnet address: %+v, %v", m, ok)
	}
	for _, c := range []struct {
		m, viewer int
		want      string
	}{{0, 1, "172.28.0.5"}, {0, 2, "127.88.0.2"}, {1, 0, "172.28.0.6"}, {2, 1, "127.88.0.4"}} {
		if a := got[c.m].SeenBy(got[c.viewer]); a != c.want {
			t.Errorf("%s seen by %s at %s, want %s", got[c.m].Names[0], got[c.viewer].Names[0], a, c.want)
		}
	}
	if !InSubnet(got, netip.MustParseAddr("172.28.9.9")) || InSubnet(got, netip.MustParseAddr("172.29.0.1")) {
		t.Error("InSubnet misreads the members' subnets")
	}
}

// A member's IPv6 addresses ride beside its IPv4 ones, found by any
// spelling, and leave its IPv4 address on each network as it was.
func TestIPv6Addresses(t *testing.T) {
	ms := []Member{
		{Address: "127.88.0.2", Names: []string{"api"}, Networks: []string{"back", "front"}, Subnets: []NetAddress{
			{"back", netip.MustParsePrefix("172.28.0.5/16")}, {"front", netip.MustParsePrefix("fd00:f::5/64")},
		}},
		{Address: "127.88.0.3", Names: []string{"db"}, Networks: []string{"back"}, Subnets: []NetAddress{{"back", netip.MustParsePrefix("172.28.0.6/16")}}},
		{Address: "127.88.0.4", Names: []string{"web"}, Networks: []string{"front"}, Subnets: []NetAddress{{"front", netip.MustParsePrefix("fd00:f::7/64")}}},
	}
	b := Format(ms)
	if !strings.HasPrefix(string(b), "127.88.0.2\tapi\t\t0\tback front\t0\t\t\tback=172.28.0.5/16 front=fd00:f::5/64\n") {
		t.Fatalf("Format:\n%s", b)
	}
	got := Parse(b)
	if !reflect.DeepEqual(got[0].Subnets, ms[0].Subnets) {
		t.Fatalf("Parse: %+v", got[0].Subnets)
	}
	if m, ok := Find(got, "fd00:f:0::7"); !ok || m.Address != "127.88.0.4" {
		t.Errorf("Find by an IPv6 address: %+v, %v", m, ok)
	}
	if m, ok := Lookup(got, "127.88.0.4", "fd00:f::5"); !ok || m.Address != "127.88.0.2" {
		t.Errorf("Lookup by an IPv6 address: %+v, %v", m, ok)
	}
	if got[0].On("front") != "127.88.0.2" || got[0].SeenBy(got[2]) != "127.88.0.2" {
		t.Errorf("an IPv6 address read as IPv4: %s", got[0].SeenBy(got[2]))
	}
	if !InSubnet(got, netip.MustParseAddr("fd00:f::99")) || InSubnet(got, netip.MustParseAddr("fd00:e::1")) {
		t.Error("InSubnet misreads the members' IPv6 subnets")
	}
}
