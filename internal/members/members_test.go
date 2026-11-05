package members

import (
	"reflect"
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
