package cri

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
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
