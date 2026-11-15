package engine

import (
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

// A tmpfs mount's options are honoured where a directory can hold them and
// refused where it cannot, never dropped.
func TestTmpfsOptions(t *testing.T) {
	for _, tc := range []struct {
		opts   string
		rw     bool
		noexec bool
		mode   uint32
		kind   Kind
	}{
		{opts: "", rw: true},
		{opts: "rw,noexec,nosuid,nodev", rw: true, noexec: true},
		{opts: "ro", rw: false},
		{opts: "noexec,exec", rw: true},
		{opts: "mode=1770", rw: true, mode: 0o1770},
		{opts: "mode=755", rw: true, mode: 0o755},
		{opts: "mode=9", kind: KindInvalid},
		{opts: "mode=4755", kind: KindInvalid},
		{opts: "size=64m", kind: KindUnsupported},
		{opts: "rw,size=65536k", kind: KindUnsupported},
		{opts: "uid=1000", kind: KindUnsupported},
		{opts: "suid", kind: KindUnsupported},
	} {
		m := mountRec{Type: mount.TypeTmpfs, RW: true}
		var err error
		for _, o := range splitOpts(tc.opts) {
			if err = tmpfsOption(&m, o); err != nil {
				break
			}
		}
		if tc.kind != 0 {
			if err == nil || KindOf(err) != tc.kind {
				t.Errorf("%q: %v, want an error of kind %v", tc.opts, err, tc.kind)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.opts, err)
			continue
		}
		var mode uint32
		if m.Mode != nil {
			mode = *m.Mode
		}
		if m.RW != tc.rw || m.NoExec != tc.noexec || mode != tc.mode {
			t.Errorf("%q: rw %v noexec %v mode %o, want %v %v %o", tc.opts, m.RW, m.NoExec, mode, tc.rw, tc.noexec, tc.mode)
		}
	}
}

func splitOpts(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

// A masked or read-only path lies in the mount that holds it, the deepest
// one, and otherwise in the root.
func TestHostPath(t *testing.T) {
	c := &Container{dir: "/s/c/1"}
	c.rec.Mounts = []mountRec{
		{Destination: "/data", Source: "/s/v/data"},
		{Destination: "/data/logs/", Source: "/s/v/logs"},
		{Destination: "/database", Source: "/s/v/db"},
	}
	for in, want := range map[string]string{
		"/proc/kcore":    "/s/c/1/r/proc/kcore",
		"/data":          "/s/v/data",
		"/data/x":        "/s/v/data/x",
		"/data/logs/a":   "/s/v/logs/a",
		"/database/../x": "/s/c/1/r/x",
		"/databasex":     "/s/c/1/r/databasex",
	} {
		if got := c.hostPath(in); got != want {
			t.Errorf("hostPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// A MAC address a client sets is refused rather than replaced.
func TestCheckMAC(t *testing.T) {
	if err := checkMAC(nil); err != nil {
		t.Error(err)
	}
	if err := checkMAC(&network.EndpointSettings{}); err != nil {
		t.Error(err)
	}
	if err := checkMAC(&network.EndpointSettings{MacAddress: network.HardwareAddr{2, 0x42, 1, 2, 3, 4}}); KindOf(err) != KindUnsupported {
		t.Errorf("a set MAC address gave %v", err)
	}
}
