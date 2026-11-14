package engine

import (
	"log"
	"sort"
	"time"

	"github.com/moby/moby/api/types/container"
)

// The memory guard keeps the containers from swapping the Mac to a halt.
// macOS reports its memory pressure as a level: normal, warning, or
// critical, where it compresses and swaps what it can and the user's own
// programs stall. While the level is critical the engine pauses, one at a
// time, the running container whose processes hold the most memory, as
// docker pause does; their memory then pages out without being touched
// again, and the user's programs get it. Once the level is back to normal
// it resumes them. It pauses rather than kills, so a container's work
// waits instead of being lost.

// Memory pressure levels, as kern.memorystatus_vm_pressure_level gives
// them.
const (
	pressureNormal   = 1
	pressureWarning  = 2
	pressureCritical = 4
)

// guardPeriod is how often the guard reads the pressure level.
const guardPeriod = 2 * time.Second

type guardCandidate struct {
	id        string
	footprint uint64
	paused    bool
}

// memoryGuard is the guard's state: the containers it paused, in order.
type memoryGuard struct {
	paused []string
}

// step decides, for the pressure level and the running containers, which
// container to pause, at most one a step, and which to resume.
func (g *memoryGuard) step(level int, running []guardCandidate) (pause string, resume []string) {
	switch level {
	case pressureCritical:
		ours := map[string]bool{}
		for _, id := range g.paused {
			ours[id] = true
		}
		cands := append([]guardCandidate(nil), running...)
		sort.Slice(cands, func(i, j int) bool { return cands[i].footprint > cands[j].footprint })
		for _, c := range cands {
			if !c.paused && !ours[c.id] {
				g.paused = append(g.paused, c.id)
				return c.id, nil
			}
		}
	case pressureNormal:
		resume, g.paused = g.paused, nil
	}
	return "", resume
}

// guardMemory runs the guard until stop closes, reading the level with
// level.
func (e *Engine) guardMemory(level func() (int, error), stop <-chan struct{}) {
	var g memoryGuard
	t := time.NewTicker(guardPeriod)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		l, err := level()
		if err != nil || (l != pressureCritical && len(g.paused) == 0) {
			continue
		}
		var running []guardCandidate
		if l == pressureCritical {
			running = e.guardCandidates()
		}
		pause, resume := g.step(l, running)
		if pause != "" {
			log.Printf("memory pressure is critical: pausing container %s, which holds the most memory, until it eases", pause[:12])
			if err := e.Pause(pause); err != nil {
				log.Printf("memory guard: pause %s: %v", pause[:12], err)
			}
		}
		for _, id := range resume {
			log.Printf("memory pressure is normal: resuming container %s", id[:12])
			if err := e.Unpause(id); err != nil {
				log.Printf("memory guard: unpause %s: %v", id[:12], err)
			}
		}
	}
}

// guardCandidates are the running containers with the memory their
// processes hold.
func (e *Engine) guardCandidates() []guardCandidate {
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
		out = append(out, guardCandidate{id: c.rec.ID, footprint: st.Footprint, paused: paused})
	}
	return out
}
