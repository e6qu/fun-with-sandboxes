package procs

import (
	"fmt"
	"os"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
)

// A container's main process is spawned with responsibility disclaimed, so
// macOS holds it responsible for itself, and holds it responsible for every
// process it and its descendants start: the kernel keeps the mark for a
// process's life, through a new session, a cleared environment and
// overwritten arguments, and no process can drop it but by being spawned
// with it disclaimed. The container's execs start from its anchor, which
// the main process's spawn helper forked, so they carry it too.

// Answers reports whether macOS holds main, a process that disclaimed its
// responsibility, responsible for p. A pid is reused once its process
// ends: p started no earlier than main did, and, when a later process has
// taken main's pid, before that one.
func Answers(p, main Entry, reused int64) bool {
	if p.PID == main.PID || p.Start < main.Start || (reused != 0 && p.Start >= reused) {
		return false
	}
	return libsystem.ResponsiblePID(p.PID) == main.PID
}

// Responsible lists this user's processes macOS holds main responsible
// for, as Answers decides, main itself aside.
func Responsible(main Entry) ([]int, error) {
	live, err := Own()
	if err != nil {
		return nil, err
	}
	var reused int64
	for _, p := range live {
		if p.PID == main.PID && p.Start != main.Start {
			reused = p.Start
		}
	}
	self := os.Getpid()
	var out []int
	for _, p := range live {
		if p.PID != self && Answers(p, main, reused) {
			out = append(out, p.PID)
		}
	}
	return out, nil
}

// KillResponsible kills every process Responsible finds and returns once
// none is left, killing what they started meanwhile too.
func KillResponsible(main Entry) error {
	return killFound(func() ([]int, error) { return Responsible(main) }, fmt.Sprintf("answering to %d", main.PID))
}
