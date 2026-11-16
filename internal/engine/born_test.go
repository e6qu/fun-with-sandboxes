package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A file is settled while it and its directory are unchanged since the
// stamp, in both trees; a write, a chmod or a rename into its directory
// after the stamp unsettles it.
func TestSettledSinceBorn(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	for _, d := range []string{lower, upper} {
		for _, f := range []string{"a/kept", "a/written", "a/chmodded", "b/kept"} {
			p := filepath.Join(d, f)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	time.Sleep(20 * time.Millisecond)
	if err := stampBorn(upper); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(upper, bornFile))
	if err != nil {
		t.Fatal(err)
	}
	born := fi.ModTime()
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(upper, "a/written"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(upper, "a/chmodded"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "b/new"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stat := func(p string) os.FileInfo {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	clones, root := settledClones(born), settledRoot(upper, born)
	for rel, want := range map[string]bool{"/a/kept": true, "/a/written": false, "/a/chmodded": false, "/b/kept": false} {
		l, u := filepath.Join(lower, rel), filepath.Join(upper, rel)
		if got := clones(rel, stat(l), stat(u), stat(filepath.Dir(l)), stat(filepath.Dir(u))); got != want {
			t.Errorf("clones: %s settled %v, want %v", rel, got, want)
		}
		if got := root(rel, nil, nil, nil, nil); got != want {
			t.Errorf("root: %s settled %v, want %v", rel, got, want)
		}
	}
	if root("/a/missing", nil, nil, nil, nil) {
		t.Error("root: a file the root lacks is settled")
	}
}
