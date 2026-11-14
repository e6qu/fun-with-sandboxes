package engine

import (
	"slices"
	"testing"
)

// Under critical pressure the guard pauses the running container holding
// the most memory, one a step, skipping those paused already; it does
// nothing at the warning level, and resumes what it paused once the level
// is normal.
func TestMemoryGuard(t *testing.T) {
	var g memoryGuard
	running := []guardCandidate{
		{id: "small", footprint: 10},
		{id: "big", footprint: 300},
		{id: "user-paused", footprint: 900, paused: true},
		{id: "mid", footprint: 100},
	}
	if p, r := g.step(pressureWarning, running); p != "" || r != nil {
		t.Fatalf("warning: pause %q resume %v", p, r)
	}
	if p, _ := g.step(pressureCritical, running); p != "big" {
		t.Fatalf("first critical step paused %q, want big", p)
	}
	running[1].paused = true
	if p, _ := g.step(pressureCritical, running); p != "mid" {
		t.Fatalf("second critical step paused %q, want mid", p)
	}
	if p, r := g.step(pressureWarning, running); p != "" || r != nil {
		t.Fatalf("warning after critical: pause %q resume %v", p, r)
	}
	if _, r := g.step(pressureNormal, running); !slices.Equal(r, []string{"big", "mid"}) {
		t.Fatalf("normal resumed %v, want what the guard paused", r)
	}
	if _, r := g.step(pressureNormal, running); r != nil {
		t.Fatalf("a second normal step resumed %v", r)
	}
}
