package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// A follower that skips the stored entries is told once it is past them,
// and an entry written from then on, such as a container's answer to
// input an attach feeds it, is one it emits.
func TestFollowEmitsWhatFollowsTheStoredEntries(t *testing.T) {
	l, err := openLogStore(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	for i := 0; i < 1000; i++ {
		l.append("stdout", "stored\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got []string
	err = l.followEntries(ctx, followOptions{follow: true, tail: 0, stored: func() {
		l.append("stdout", "answer\n")
	}}, func(e LogEntry) error {
		got = append(got, e.Log)
		cancel()
		return nil
	})
	if err != nil || len(got) != 1 || got[0] != "answer\n" {
		t.Fatalf("followed %q, %v; want the entry written once the stored ones were passed", got, err)
	}
}
