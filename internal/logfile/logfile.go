// Package logfile writes a container's output as Docker's json-file log
// driver stores it: one JSON object per line, each with its stream, its
// text and its time.
package logfile

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"time"
)

type Entry struct {
	Stream string    `json:"stream"`
	Log    string    `json:"log"`
	Time   time.Time `json:"time"`
}

// Appender appends entries to a log file. Several processes may append to
// one file: each entry is one write to a file opened for appending.
type Appender struct {
	mu sync.Mutex
	f  *os.File
	// Notify, if set, runs after each entry is written.
	Notify func()
}

func Open(path string) (*Appender, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Appender{f: f}, nil
}

func (a *Appender) Append(stream, line string) error {
	b, err := json.Marshal(Entry{Stream: stream, Log: line, Time: time.Now().UTC()})
	if err != nil {
		return err
	}
	a.mu.Lock()
	_, err = a.f.Write(append(b, '\n'))
	a.mu.Unlock()
	if a.Notify != nil {
		a.Notify()
	}
	return err
}

func (a *Appender) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.f.Close()
}

// LineWriter turns a byte stream into log entries, one per line, keeping
// the newline as Docker's json-file driver does.
type LineWriter struct {
	Stream string
	To     *Appender
	mu     sync.Mutex
	buf    []byte
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.To.Append(w.Stream, string(w.buf[:i+1]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// Flush writes a final line without a newline.
func (w *LineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.To.Append(w.Stream, string(w.buf))
		w.buf = nil
	}
}
