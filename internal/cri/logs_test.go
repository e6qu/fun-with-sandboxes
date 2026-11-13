package cri

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// A line the engine stored whole is F in CRI's log, a part of one P, and
// the file's last line tells a resumed copy where to start.
func TestCRILogFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod", "c", "0.log")
	l := &logCopy{path: path}
	if err := l.open(); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 8, 21, 0, 0, 123456789, time.UTC)
	t1 := t0.Add(time.Second)
	for _, le := range []engine.LogEntry{
		{Stream: "stdout", Log: "whole line\n", Time: t0},
		{Stream: "stderr", Log: "no newline yet", Time: t1},
	} {
		if err := l.write(le); err != nil {
			t.Fatal(err)
		}
	}
	l.f.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-11-08T21:00:00.123456789Z stdout F whole line\n" +
		"2026-11-08T21:00:01.123456789Z stderr P no newline yet\n"
	if string(b) != want {
		t.Errorf("the CRI log:\n%q\nwant\n%q", b, want)
	}
	if got := lastLogged(path); !got.Equal(t1) {
		t.Errorf("the last logged time: %v, want %v", got, t1)
	}
	if got := lastLogged(filepath.Join(t.TempDir(), "none.log")); !got.IsZero() {
		t.Errorf("a missing log's last time: %v", got)
	}
}

func TestMatchesSelector(t *testing.T) {
	labels := map[string]string{"app": "web", "tier": "front"}
	for sel, want := range map[string]bool{"": true, "app=web": true, "app=db": false, "zone=a": false} {
		selector := map[string]string{}
		if sel != "" {
			k, v, _ := strings.Cut(sel, "=")
			selector[k] = v
		}
		if got := matchesSelector(labels, selector); got != want {
			t.Errorf("selector %q: %v, want %v", sel, got, want)
		}
	}
}

// Close stops the log copies without holding the lock a copy that ends
// takes to forget itself, as an engine nested in a node closes its CRI
// while its pods' logs are copied.
func TestCloseStopsLogCopies(t *testing.T) {
	s := &Service{logs: map[string]*logCopy{}}
	for _, id := range []string{"a", "b"} {
		ctx, cancel := context.WithCancel(context.Background())
		l := &logCopy{cancel: cancel, done: make(chan struct{})}
		s.logs[id] = l
		go func() {
			defer close(l.done)
			<-ctx.Done()
			s.mu.Lock()
			if s.logs[id] == l {
				delete(s.logs, id)
			}
			s.mu.Unlock()
		}()
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	if len(s.logs) != 0 {
		t.Errorf("logs left: %v", s.logs)
	}
}

// ImageFsInfo's measure of the containers' files never waits on the walk:
// it answers with the latest, and starts the next once that is old.
func TestUsageCacheMeasuresInTheBackground(t *testing.T) {
	var c usageCache
	release := make(chan struct{})
	measured := make(chan struct{}, 2)
	measure := func(string) image.Usage {
		<-release
		measured <- struct{}{}
		return image.Usage{Bytes: 42, Inodes: 3}
	}
	start := time.Now()
	if u := c.get("/x", measure); u != (image.Usage{}) || time.Since(start) > time.Second {
		t.Fatalf("the first request %+v waited on the measure", u)
	}
	// A second request while the first measure runs starts no other.
	c.get("/x", measure)
	close(release)
	<-measured
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if u := c.get("/x", measure); u == (image.Usage{Bytes: 42, Inodes: 3}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the measure never landed")
		}
	}
	select {
	case <-measured:
		t.Fatal("a fresh measure was taken again")
	default:
	}
}
