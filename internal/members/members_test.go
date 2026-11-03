package members

import (
	"reflect"
	"testing"
)

func TestFormatParseFind(t *testing.T) {
	ms := []Member{
		{Address: "127.88.0.2", Names: []string{"proj-db-1", "db"}, Ports: map[string]int{"5432/tcp": 30010}},
		{Address: "127.88.0.3", Names: []string{"web"}, Ports: map[string]int{"80/tcp": 30011, "53/udp": 30012}},
	}
	b := Format(ms)
	if want := "127.88.0.2\tproj-db-1 db\t5432/tcp=30010\n127.88.0.3\tweb\t53/udp=30012,80/tcp=30011\n"; string(b) != want {
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
	if _, ok := Find(ms, "example.com"); ok {
		t.Error("a name no member answers to was found")
	}
}
