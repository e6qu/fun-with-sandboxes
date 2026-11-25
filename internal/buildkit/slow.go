package buildkit

import (
	"log"
	"time"
)

// slowStep is how long a step of a build can take before the engine's log
// names it: a build that stalls shows there which step it waited on.
const slowStep = 200 * time.Millisecond

// slow logs what, begun at began, when it took slowStep or longer; a step
// defers it.
func slow(began time.Time, format string, a ...any) {
	if took := time.Since(began); took >= slowStep {
		log.Printf("buildkit: "+format+" took %s", append(a, took.Round(time.Millisecond))...)
	}
}
