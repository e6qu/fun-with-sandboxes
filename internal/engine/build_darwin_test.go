package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/e6qu/fun-with-sandboxes/internal/dockerfile"
)

// A step that changes files under --no-cache records when it ran: the
// image's history, and so its creation time, carry it.
func TestNoCacheStepKeepsItsTime(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "s0")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	b := &builder{e: &Engine{}, opts: BuildOptions{NoCache: true}, dir: dir, progress: func(string) {}, cache: &buildCache{dir: filepath.Join(dir, "cache")}}
	s := &stage{root: root, args: map[string]string{}, shell: []string{"/bin/sh", "-c"}, key: cacheKey("scratch")}
	if err := b.apply(context.Background(), s, dockerfile.Instruction{Cmd: "WORKDIR", Raw: "/app", Line: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if h := s.cfg.History; len(h) != 1 || h[0].Created.IsZero() || h[0].EmptyLayer {
		t.Errorf("history after WORKDIR made a directory: %+v", h)
	}
}
