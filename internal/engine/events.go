package engine

import (
	"sync"
	"time"

	"github.com/moby/moby/api/types/events"
)

// eventBus fans engine events out to /events subscribers and keeps a short
// history for `since` queries.
type eventBus struct {
	mu      sync.Mutex
	subs    map[chan events.Message]struct{}
	history []events.Message
}

const eventHistory = 1024

func newEventBus() *eventBus {
	return &eventBus{subs: map[chan events.Message]struct{}{}}
}

func (b *eventBus) publish(typ events.Type, action events.Action, id string, attrs map[string]string) {
	now := time.Now()
	m := events.Message{
		Type:     typ,
		Action:   action,
		Actor:    events.Actor{ID: id, Attributes: attrs},
		Scope:    "local",
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.history = append(b.history, m)
	if len(b.history) > eventHistory {
		b.history = b.history[len(b.history)-eventHistory:]
	}
	for ch := range b.subs {
		select {
		case ch <- m:
		default:
			// A subscriber that stops reading loses events rather than
			// stalling the engine.
		}
	}
}

// Subscribe returns past events newer than since and a channel of new ones.
func (e *Engine) Subscribe(since time.Time) ([]events.Message, <-chan events.Message, func()) {
	b := e.events
	ch := make(chan events.Message, 256)
	b.mu.Lock()
	var past []events.Message
	if !since.IsZero() {
		for _, m := range b.history {
			if m.TimeNano >= since.UnixNano() {
				past = append(past, m)
			}
		}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return past, ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}
