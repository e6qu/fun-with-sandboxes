package engine

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/shim"
)

// A nested engine publishes TCP and UDP ports in its privileged container:
// on the container's port's host port where it has one, else in its port
// directory.
func TestNestedBinding(t *testing.T) {
	n := &Nested{Port: func(proto string, p int) (int, string, error) {
		if p == 53 && proto == "udp" {
			return 40053, "", nil
		}
		return 0, "/run/p", nil
	}}
	any := netip.IPv4Unspecified()
	loop := netip.MustParseAddr("127.0.0.1")
	for _, tc := range []struct {
		port   string
		lo, hi int
		want   published
	}{
		{"53/udp", 53, 53, published{report: 53, binding: shim.Binding{Proto: "udp", IP: loop, Lo: 40053, Hi: 40053, Upstream: "127.0.0.1:9"}}},
		{"5353/udp", 5353, 5353, published{binding: shim.Binding{Proto: "udp", IP: any, Lo: 5353, Hi: 5353, Upstream: "127.0.0.1:9", Dir: "/run/p"}}},
		{"8080/tcp", 0, 0, published{binding: shim.Binding{Proto: "tcp", IP: any, Upstream: "127.0.0.1:9", Dir: "/run/p"}}},
	} {
		p := network.MustParsePort(tc.port)
		got, err := nestedBinding(n, p, any, tc.lo, tc.hi, "127.0.0.1:9")
		if err != nil {
			t.Fatalf("%s: %v", tc.port, err)
		}
		tc.want.port = p
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.port, got, tc.want)
		}
	}
	if _, err := nestedBinding(n, network.MustParsePort("9/sctp"), any, 9, 9, "127.0.0.1:9"); err == nil {
		t.Error("an SCTP port published in a privileged container")
	}
}

// An update's block I/O weight reaches the container's file as the tier it
// maps to, which the container library sets its new processes to.
func TestWriteIOPolicy(t *testing.T) {
	c := &Container{dir: t.TempDir()}
	for weight, want := range map[uint16]string{1000: "5\n", 300: "4\n", 10: "3\n"} {
		if err := c.writeIOPolicy(weight); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(c.ioPolicyPath())
		if err != nil || string(b) != want {
			t.Errorf("weight %d wrote %q, %v; want %q", weight, b, err, want)
		}
	}
	if filepath.Dir(c.ioPolicyPath()) != filepath.Dir(c.pidsPath()) {
		t.Errorf("the I/O policy's file %s is not beside the pids limit's, where the library looks", c.ioPolicyPath())
	}
}
