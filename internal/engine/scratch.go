package engine

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A container's root is a clone of its image's, which takes as long to
// make and to delete as the image has files. The engine clones into
// staging and deletes from trash without its lock, and moves a root into
// or out of place under it with a rename.

func stagingDir(root string) string { return filepath.Join(root, "staging") }
func trashDir(root string) string   { return filepath.Join(root, "trash") }

// clearScratch makes the staging and trash directories, and deletes what
// an earlier engine left in them, in the background.
func clearScratch(root string) error {
	for _, d := range []string{stagingDir(root), trashDir(root)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			return err
		}
		for _, en := range entries {
			go os.RemoveAll(filepath.Join(d, en.Name()))
		}
	}
	return nil
}

// discard moves dir to the trash and deletes it in the background.
func (e *Engine) discard(dir string) error {
	t, err := os.MkdirTemp(trashDir(e.cfg.Root), filepath.Base(dir)+"-")
	if err != nil {
		return err
	}
	if err := os.Rename(dir, filepath.Join(t, "d")); err != nil {
		os.Remove(t)
		return err
	}
	go os.RemoveAll(t)
	return nil
}

// steps times the steps of an operation on a container for the debug log.
type steps struct {
	e     *Engine
	what  string
	start time.Time
	last  time.Time
	parts []string
}

func (e *Engine) steps(what string) *steps {
	now := time.Now()
	return &steps{e: e, what: what, start: now, last: now}
}

// mark ends the step named step.
func (s *steps) mark(step string) {
	if !s.e.cfg.Debug {
		return
	}
	now := time.Now()
	s.parts = append(s.parts, step+" "+now.Sub(s.last).Round(time.Microsecond).String())
	s.last = now
}

// log logs the steps, with extra details.
func (s *steps) log(extra ...string) {
	if !s.e.cfg.Debug {
		return
	}
	log.Printf("%s took %s: %s", s.what, time.Since(s.start).Round(time.Microsecond), strings.Join(append(s.parts, extra...), ", "))
}
