package engine

import (
	"log"
	"sort"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/e6qu/fun-with-sandboxes/internal/shim"
)

// The guards keep the containers from taking the Mac down. macOS reports
// its memory pressure as a level: normal, warning, or critical, where it
// compresses and swaps what it can and the user's own programs stall; and
// a disk that fills stops every program that writes. While a guard's
// measure is critical, the engine pauses, one at a time, the running
// container that uses the most of it, as docker pause does: a paused
// container's memory pages out without being touched again, and it writes
// no more. Once the measure is back to normal, the guard resumes the
// containers it paused. It pauses rather than kills, so a container's work
// waits instead of being lost.

type level int

const (
	levelNormal level = iota
	levelWarning
	levelCritical
)

// Memory pressure levels, as kern.memorystatus_vm_pressure_level gives
// them.
const (
	pressureNormal   = 1
	pressureWarning  = 2
	pressureCritical = 4
)

// pressure maps a memory pressure level to a guard's.
func pressure(l int) level {
	switch l {
	case pressureCritical:
		return levelCritical
	case pressureWarning:
		return levelWarning
	}
	return levelNormal
}

// The disk guard's thresholds: critical below diskLow free, normal from
// twice that.
const diskLow = 2 << 30

// diskLevel is the guard's level for a disk with free bytes available.
func diskLevel(free uint64) level {
	switch {
	case free < diskLow:
		return levelCritical
	case free < 2*diskLow:
		return levelWarning
	}
	return levelNormal
}

// guardPeriod is how often the guards read their measures.
const guardPeriod = 2 * time.Second

type guardCandidate struct {
	id     string
	use    uint64
	paused bool
}

// pauseGuard is one guard's state: the containers it paused, in order.
type pauseGuard struct {
	paused []string
}

// step decides, for the guard's level and the running containers, which
// container to pause, at most one a step, and which to resume. It leaves
// alone the containers in skip, which another guard paused.
func (g *pauseGuard) step(l level, running []guardCandidate, skip map[string]bool) (pause string, resume []string) {
	switch l {
	case levelCritical:
		ours := map[string]bool{}
		for _, id := range g.paused {
			ours[id] = true
		}
		cands := append([]guardCandidate(nil), running...)
		sort.Slice(cands, func(i, j int) bool { return cands[i].use > cands[j].use })
		for _, c := range cands {
			if !c.paused && !ours[c.id] && !skip[c.id] {
				g.paused = append(g.paused, c.id)
				return c.id, nil
			}
		}
	case levelNormal:
		resume, g.paused = g.paused, nil
	}
	return "", resume
}

// guard is a measure the engine guards: its level, and a container's use
// of it.
type guard struct {
	name  string
	level func() (level, error)
	use   func(shim.State) uint64
	pauseGuard
}

// guardMachine runs the guards until stop closes.
func (e *Engine) guardMachine(guards []*guard, stop <-chan struct{}) {
	t := time.NewTicker(guardPeriod)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		for _, g := range guards {
			l, err := g.level()
			if err != nil || (l != levelCritical && len(g.paused) == 0) {
				continue
			}
			var running []guardCandidate
			if l == levelCritical {
				running = e.guardCandidates(g.use)
			}
			skip := map[string]bool{}
			for _, o := range guards {
				if o != g {
					for _, id := range o.paused {
						skip[id] = true
					}
				}
			}
			pause, resume := g.step(l, running, skip)
			if pause != "" {
				log.Printf("%s is critical: pausing container %s, which uses the most of it, until it eases", g.name, pause[:12])
				if err := e.Pause(pause); err != nil {
					log.Printf("%s guard: pause %s: %v", g.name, pause[:12], err)
				}
			}
			for _, id := range resume {
				log.Printf("%s is normal: resuming container %s", g.name, id[:12])
				if err := e.Unpause(id); err != nil {
					log.Printf("%s guard: unpause %s: %v", g.name, id[:12], err)
				}
			}
		}
	}
}

// guardCandidates are the running containers with their use of a measure.
func (e *Engine) guardCandidates(use func(shim.State) uint64) []guardCandidate {
	e.mu.Lock()
	cs := make([]*Container, 0, len(e.containers))
	for _, c := range e.containers {
		cs = append(cs, c)
	}
	e.mu.Unlock()
	var out []guardCandidate
	for _, c := range cs {
		c.mu.Lock()
		task, running, paused := c.task, c.rec.State.Status == container.StateRunning, c.rec.State.Paused
		c.mu.Unlock()
		if task == nil || !running {
			continue
		}
		st, err := task.State()
		if err != nil {
			continue
		}
		out = append(out, guardCandidate{id: c.rec.ID, use: use(st), paused: paused})
	}
	return out
}

// machineGuards are the guards of an engine whose state is under root.
func machineGuards(root string) []*guard {
	return []*guard{
		{
			name: "memory pressure",
			level: func() (level, error) {
				l, err := pressureLevel()
				return pressure(l), err
			},
			use: func(st shim.State) uint64 { return st.Footprint },
		},
		{
			name: "free disk space",
			level: func() (level, error) {
				free, err := diskFree(root)
				return diskLevel(free), err
			},
			use: func(st shim.State) uint64 { return st.DiskWritten },
		},
	}
}
