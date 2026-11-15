package router

import (
	"bytes"
	"testing"
)

// A container library reads the client header the router writes before
// each stream; a short or odd one is an error, never a crash.
func FuzzReadClient(f *testing.F) {
	f.Add([]byte{127, 0, 0, 1, 0x4e, 0x20})
	f.Add([]byte{1, 2, 3})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		a, err := ReadClient(bytes.NewReader(b))
		if err == nil && (len(b) < ClientHeader || !a.Addr().Is4()) {
			t.Fatalf("%x read as %v", b, a)
		}
		readPort(bytes.NewReader(b))
	})
}
