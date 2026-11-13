package engine

import (
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"
)

// lockLogAfter is how long a wait for the engine's lock, or a hold of it,
// takes before an engine run with -debug logs it.
const lockLogAfter = 100 * time.Millisecond

// timedMutex is the engine's lock. With timed set, as -debug sets it, it
// logs a wait for it or a hold of it that lasts past lockLogAfter, with the
// function that waited or held it: most requests take the lock, so a slow
// one may be waiting on another's.
type timedMutex struct {
	mu    sync.Mutex
	timed bool
	// holder and since are the function that holds the lock and when it
	// took it.
	holder string
	since  time.Time
}

func (m *timedMutex) Lock() {
	if !m.timed {
		m.mu.Lock()
		return
	}
	asked := time.Now()
	m.mu.Lock()
	m.since = time.Now()
	m.holder = caller(2)
	if waited := m.since.Sub(asked); waited > lockLogAfter {
		log.Printf("engine lock: %s waited %s for it", m.holder, waited.Round(time.Millisecond))
	}
}

func (m *timedMutex) Unlock() {
	if m.timed {
		if held := time.Since(m.since); held > lockLogAfter {
			log.Printf("engine lock: %s held it %s", m.holder, held.Round(time.Millisecond))
		}
	}
	m.mu.Unlock()
}

// caller names the function skip frames up the stack, with its line.
func caller(skip int) string {
	pc, _, line, ok := runtime.Caller(skip)
	if !ok {
		return "?"
	}
	name := "?"
	if f := runtime.FuncForPC(pc); f != nil {
		name = f.Name()
	}
	return fmt.Sprintf("%s:%d", name, line)
}
