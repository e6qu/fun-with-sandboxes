package members

import (
	"reflect"
	"testing"
)

func TestFormatParseFind(t *testing.T) {
	ms := []Member{
		{Address: "127.88.0.2", Names: []string{"proj-db-1", "db"}, Ports: map[string]int{"5432/tcp": 30010}, Router: 30013, Networks: []string{"n1"}},
		{Address: "127.88.0.3", Names: []string{"web"}, Ports: map[string]int{"80/tcp": 30011, "53/udp": 30012}, Router: 30014, Networks: []string{"n1", "n2"}, Foreign: true, Stack: "other"},
	}
	b := Format(ms)
	if want := "127.88.0.2\tproj-db-1 db\t5432/tcp=30010\t30013\tn1\t0\t\n127.88.0.3\tweb\t53/udp=30012,80/tcp=30011\t30014\tn1 n2\t1\tother\n"; string(b) != want {
		t.Fatalf("Format:\n%s\nwant:\n%s", b, want)
	}
	if got := Parse(append(b, "garbage\n"...)); !reflect.DeepEqual(got, ms) {
		t.Fatalf("Parse: %+v", got)
	}
	for host, want := range map[string]string{"DB.": "127.88.0.2", "web": "127.88.0.3", "127.88.0.3": "127.88.0.3"} {
		if m, ok := Find(ms, host); !ok || m.Address != want {
			t.Errorf("Find(%q): %+v, %v", host, m, ok)
		}
	}
	if !Share(ms[0], ms[1]) || Share(ms[0], Member{Networks: []string{"n2"}}) {
		t.Error("Share misreads the members' networks")
	}
	if _, ok := Find(ms, "example.com"); ok {
		t.Error("a name no member answers to was found")
	}
}
