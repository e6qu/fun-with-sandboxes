package procs

// Linux has no responsible process; a container's processes are found by
// their group and their tag alone.

// Answers reports false.
func Answers(p, main Entry, reused int64) bool { return false }

// Responsible lists no process.
func Responsible(main Entry) ([]int, error) { return nil, nil }

// KillResponsible kills no process.
func KillResponsible(main Entry) error { return nil }
