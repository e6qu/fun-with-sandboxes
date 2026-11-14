package runtime

import "testing"

// The fork ceiling leaves the user an eighth of their processes, and at
// least 64.
func TestForkCeiling(t *testing.T) {
	for _, c := range []struct{ max, want uint64 }{{2666, 2333}, {4000, 3500}, {300, 236}, {64, 64}, {10, 10}} {
		if got := ForkCeiling(c.max); got != c.want {
			t.Errorf("ForkCeiling(%d) = %d, want %d", c.max, got, c.want)
		}
	}
}
