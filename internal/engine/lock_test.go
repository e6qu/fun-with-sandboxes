package engine

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// A timed lock logs a hold and a wait past lockLogAfter, naming the
// functions; an untimed one logs nothing.
func TestTimedMutexLogsLongHoldsAndWaits(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	for _, timed := range []bool{false, true} {
		buf.Reset()
		m := &timedMutex{timed: timed}
		held := make(chan struct{})
		done := make(chan struct{})
		go func() {
			holdTheLock(m, held)
			close(done)
		}()
		<-held
		m.Lock()
		m.Unlock()
		<-done
		out := buf.String()
		if !timed {
			if out != "" {
				t.Errorf("an untimed lock logged:\n%s", out)
			}
			continue
		}
		if !strings.Contains(out, "holdTheLock") || !strings.Contains(out, "held it") {
			t.Errorf("the hold is not logged with its holder:\n%s", out)
		}
		if !strings.Contains(out, "TestTimedMutexLogsLongHoldsAndWaits") || !strings.Contains(out, "waited") {
			t.Errorf("the wait is not logged with its waiter:\n%s", out)
		}
	}
}

func holdTheLock(m *timedMutex, held chan<- struct{}) {
	m.Lock()
	close(held)
	time.Sleep(lockLogAfter + 50*time.Millisecond)
	m.Unlock()
}
