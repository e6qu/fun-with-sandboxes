package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// LogEntry is one line of container output, stored like Docker's json-file
// log driver stores it.
type LogEntry struct {
	Stream string    `json:"stream"`
	Log    string    `json:"log"`
	Time   time.Time `json:"time"`
}

// logStore appends a container's output to its log file and wakes followers,
// which read new entries from the file at their own pace.
type logStore struct {
	path string
	mu   sync.Mutex
	f    *os.File
	subs map[chan struct{}]struct{}
}

func openLogStore(path string) (*logStore, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &logStore{path: path, f: f, subs: map[chan struct{}]struct{}{}}, nil
}

func (l *logStore) append(stream, line string) {
	b, _ := json.Marshal(LogEntry{Stream: stream, Log: line, Time: time.Now().UTC()})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.Write(append(b, '\n'))
	for ch := range l.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (l *logStore) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.Close()
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

// lineWriter turns a byte stream into log lines, keeping the newline as
// Docker's json-file driver does.
type lineWriter struct {
	stream string
	store  *logStore
	mu     sync.Mutex
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.store.append(w.stream, string(w.buf[:i+1]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.store.append(w.stream, string(w.buf))
		w.buf = nil
	}
}
