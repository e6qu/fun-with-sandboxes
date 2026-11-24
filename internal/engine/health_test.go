package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

// A CMD-SHELL test runs its words as one script, and one of the word
// alone runs an empty script rather than crashing the engine.
func TestHealthArgv(t *testing.T) {
	for _, c := range []struct{ test, want []string }{
		{[]string{"CMD", "pg_isready", "-q"}, []string{"pg_isready", "-q"}},
		{[]string{"CMD-SHELL", "curl -f localhost"}, []string{"/bin/sh", "-c", "curl -f localhost"}},
		{[]string{"CMD-SHELL", "exit", "0"}, []string{"/bin/sh", "-c", "exit 0"}},
		{[]string{"CMD-SHELL"}, []string{"/bin/sh", "-c", ""}},
	} {
		if got := healthArgv(c.test); !slices.Equal(got, c.want) {
			t.Errorf("healthArgv(%q) = %q, want %q", c.test, got, c.want)
		}
	}
}

// Create refuses the health checks dockerd refuses.
func TestCheckHealthcheck(t *testing.T) {
	for _, c := range []struct {
		hc *container.HealthConfig
		ok bool
	}{
		{nil, true},
		{&container.HealthConfig{Test: []string{"NONE"}}, true},
		{&container.HealthConfig{Interval: time.Millisecond, Timeout: time.Second, Retries: 3}, true},
		{&container.HealthConfig{Interval: time.Microsecond}, false},
		{&container.HealthConfig{Timeout: -time.Second}, false},
		{&container.HealthConfig{StartInterval: time.Nanosecond}, false},
		{&container.HealthConfig{Retries: -1}, false},
	} {
		err := checkHealthcheck(c.hc)
		if (err == nil) != c.ok || (err != nil && KindOf(err) != KindInvalid) {
			t.Errorf("checkHealthcheck(%+v) = %v", c.hc, err)
		}
	}
}
