package engine

import (
	"slices"
	"testing"
)

// At a critical level a guard pauses the running container using the most
// of its measure, one a step, skipping those paused already and those
// another guard paused; it does nothing at the warning level, and resumes
// what it paused once the level is normal.
func TestPauseGuard(t *testing.T) {
	var g pauseGuard
	running := []guardCandidate{
		{id: "small", use: 10},
		{id: "big", use: 300},
		{id: "user-paused", use: 900, paused: true},
		{id: "other-guards", use: 500},
		{id: "mid", use: 100},
	}
	skip := map[string]bool{"other-guards": true}
	if p, r := g.step(levelWarning, running, skip); p != "" || r != nil {
		t.Fatalf("warning: pause %q resume %v", p, r)
	}
	if p, _ := g.step(levelCritical, running, skip); p != "big" {
		t.Fatalf("first critical step paused %q, want big", p)
	}
	running[1].paused = true
	if p, _ := g.step(levelCritical, running, skip); p != "mid" {
		t.Fatalf("second critical step paused %q, want mid", p)
	}
	if p, r := g.step(levelWarning, running, skip); p != "" || r != nil {
		t.Fatalf("warning after critical: pause %q resume %v", p, r)
	}
	if _, r := g.step(levelNormal, running, skip); !slices.Equal(r, []string{"big", "mid"}) {
		t.Fatalf("normal resumed %v, want what the guard paused", r)
	}
	if _, r := g.step(levelNormal, running, skip); r != nil {
		t.Fatalf("a second normal step resumed %v", r)
	}
}

// The levels the guards read: macOS's memory pressure, and free disk space
// against the threshold, with room between critical and normal.
func TestGuardLevels(t *testing.T) {
	for l, want := range map[int]level{pressureNormal: levelNormal, pressureWarning: levelWarning, pressureCritical: levelCritical} {
		if got := pressure(l); got != want {
			t.Errorf("pressure(%d) = %d, want %d", l, got, want)
		}
	}
	for free, want := range map[uint64]level{diskLow - 1: levelCritical, diskLow: levelWarning, 2*diskLow - 1: levelWarning, 2 * diskLow: levelNormal} {
		if got := diskLevel(free); got != want {
			t.Errorf("diskLevel(%d) = %d, want %d", free, got, want)
		}
	}
}
