package engine

import (
	"testing"
	"time"
)

func TestParseTimestamp(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"90m":                  now.Add(-90 * time.Minute),
		"2025-01-02T03:04:05Z": time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
		"1700000000":           time.Unix(1700000000, 0),
		"1700000000.5":         time.Unix(1700000000, 500000000),
	} {
		got, err := parseTimestamp(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseTimestamp(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseTimestamp("yesterday", now); err == nil {
		t.Error("parseTimestamp accepted yesterday")
	}
}

// Name filters are regular expressions, as Docker's are; a container's
// names match with and without their leading slash.
func TestNameFilterMatchesRegexps(t *testing.T) {
	for _, c := range []struct {
		want  string
		match bool
	}{
		{"web", true}, {"^/proj-web-1$", true}, {"^proj-web-1$", true}, {"^web$", false}, {"web-[0-9]$", true}, {"[", false},
	} {
		if got := (Filters{"name": {c.want}}).matchName("name", "/proj-web-1", "proj-web-1"); got != c.match {
			t.Errorf("name=%s: %v, want %v", c.want, got, c.match)
		}
	}
	if !(Filters{}).matchName("name", "x") {
		t.Error("an absent filter refused a name")
	}
}
