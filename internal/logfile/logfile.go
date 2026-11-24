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

// maxLine is the longest entry LineWriter writes: dockerd splits a longer
// line into entries of this length, the last one holding its newline.
const maxLine = 16 * 1024

// LineWriter turns a byte stream into log entries, one per line, keeping
// the newline as Docker's json-file driver does. Write always succeeds: a
// line the log fails to take is lost, as dockerd's are, rather than stall
// the container's output.
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
		n := bytes.IndexByte(w.buf, '\n') + 1
		if n == 0 || n > maxLine {
			if len(w.buf) < maxLine {
				return len(p), nil
			}
			n = maxLine
		}
		w.To.Append(w.Stream, string(w.buf[:n]))
		w.buf = w.buf[n:]
	}
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
