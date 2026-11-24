package bandwidth

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseRate(t *testing.T) {
	for in, want := range map[string]int64{"10M": 10e6, "1k": 1000, "512Ki": 512 << 10, "1.5G": 15e8, "2000": 2000} {
		if got, err := ParseRate(in); err != nil || got != want {
			t.Errorf("ParseRate(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "M", "-1M", "fast", "999", "10Mbps", "NaN", "Inf", "1e300", "2P"} {
		if _, err := ParseRate(in); err == nil {
			t.Errorf("ParseRate(%q) accepted", in)
		}
	}
}

// A copy at 800k bits a second, 100 kB a second, of 150 kB past the
// bucket's first 32 KiB takes over a second.
func TestCopyHoldsTheRate(t *testing.T) {
	l := NewLimiter(800e3)
	var out bytes.Buffer
	start := time.Now()
	n, err := Copy(&out, strings.NewReader(strings.Repeat("x", 150_000)), l)
	if err != nil || n != 150_000 || out.Len() != 150_000 {
		t.Fatalf("copied %d, %v", n, err)
	}
	if d := time.Since(start); d < time.Second || d > 3*time.Second {
		t.Errorf("150 kB at 100 kB a second took %v", d)
	}
	if NewLimiter(0) != nil {
		t.Error("a zero rate made a limiter")
	}
}
