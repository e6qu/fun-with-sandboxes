package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"

	"github.com/e6qu/fun-with-sandboxes/internal/logfile"
)

// LogEntry is one line of container output, stored like Docker's json-file
// log driver stores it.
type LogEntry = logfile.Entry

// logStore is a container's log file, which its shim appends the
// container's output to and the engine its own notes; it wakes followers,
// which read new entries from the file at their own pace.
type logStore struct {
	path string
	app  *logfile.Appender
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func openLogStore(path string) (*logStore, error) {
	app, err := logfile.Open(path)
	if err != nil {
		return nil, err
	}
	l := &logStore{path: path, app: app, subs: map[chan struct{}]struct{}{}}
	app.Notify = l.wake
	return l, nil
}

func (l *logStore) append(stream, line string) {
	l.app.Append(stream, line)
}

// wake tells followers the file has grown.
func (l *logStore) wake() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ch := range l.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (l *logStore) close() {
	l.app.Close()
}

type followOptions struct {
	follow bool
	// done ends following once the file is drained.
	done <-chan struct{}
	// tail limits the stored entries emitted to the last n; negative emits
	// all of them.
	tail int
	want func(LogEntry) bool
}

// followEntries calls fn for the stored entries that want accepts and, when following,
// for each new one until ctx ends or done closes and the file is drained.
func (l *logStore) followEntries(ctx context.Context, o followOptions, fn func(LogEntry) error) error {
	wake := make(chan struct{}, 1)
	if o.follow {
		l.mu.Lock()
		l.subs[wake] = struct{}{}
		l.mu.Unlock()
		defer func() {
			l.mu.Lock()
			delete(l.subs, wake)
			l.mu.Unlock()
		}()
	}
	f, err := os.Open(l.path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	var partial []byte
	drain := func(emit func(LogEntry) error) error {
		for {
			chunk, err := r.ReadBytes('\n')
			partial = append(partial, chunk...)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			var e LogEntry
			if err := json.Unmarshal(bytes.TrimSpace(partial), &e); err != nil {
				return err
			}
			partial = partial[:0]
			if o.want != nil && !o.want(e) {
				continue
			}
			if err := emit(e); err != nil {
				return err
			}
		}
	}
	if o.tail >= 0 {
		var kept []LogEntry
		if err := drain(func(e LogEntry) error {
			kept = append(kept, e)
			if len(kept) > o.tail {
				kept = kept[1:]
			}
			return nil
		}); err != nil {
			return err
		}
		for _, e := range kept {
			if err := fn(e); err != nil {
				return err
			}
		}
	} else if err := drain(fn); err != nil {
		return err
	}
	if !o.follow {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-o.done:
			return drain(fn)
		case <-wake:
			if err := drain(fn); err != nil {
				return err
			}
		}
	}
}
