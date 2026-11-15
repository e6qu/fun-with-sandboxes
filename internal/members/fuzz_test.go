package members

import (
	"reflect"
	"testing"
)

// Every container's library and router read their stack's network file,
// so no content may crash the reader, and what it reads, written again,
// reads the same.
func FuzzParse(f *testing.F) {
	f.Add([]byte("127.88.0.2\tdb db-1\t5432=20001\t20050\tdefault\t0\tapp\n"))
	f.Add([]byte("127.88.0.3\tweb\t80=20002,443=20003\t20051\tdefault front\t1\tother\tweb/alias\tdefault=172.18.0.2/16\t20100-20200\n"))
	f.Add([]byte("\t\t\t\t\t\n\n127.88.0.4\tx\t=\tnope\t\t0\n"))
	f.Add([]byte("a\tb\tc=d,e\tf\tg\th\ti\tj/k\tn=::1/128 m=bad\t5-1\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		first := Parse(b)
		again := Parse(Format(first))
		if len(first) == 0 && len(again) == 0 {
			return
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("written again, %q reads\n%#v\nwhere it read\n%#v", b, again, first)
		}
	})
}
