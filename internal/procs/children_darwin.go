package procs

import "github.com/e6qu/fun-with-sandboxes/internal/libsystem"

// Children are ppid's live children, read one by one: the kernel lists
// their pids, so following a process's forks costs no listing of every
// process.
func Children(ppid int) ([]Entry, error) {
	pids, err := libsystem.ChildPIDs(ppid)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(pids))
	for _, pid := range pids {
		if e, ok := Find(pid); ok && e.PPID == ppid {
			out = append(out, e)
		}
	}
	return out, nil
}
