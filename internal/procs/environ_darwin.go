package procs

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func ownPIDs() ([]int, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, fmt.Errorf("procs: list processes: %w", err)
	}
	out := make([]int, 0, len(kps))
	for _, kp := range kps {
		out = append(out, int(kp.Proc.P_pid))
	}
	return out, nil
}

// Environ returns the environment pid started with, from the kernel's copy
// of its arguments, kern.procargs2.
func Environ(pid int) ([]string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return parseProcargs(b)
}

// sZomb is a zombie's p_stat (sys/proc.h).
const sZomb = 5

// Own lists the user's live processes from one kern.proc.uid sysctl, without
// starting ps.
func Own() ([]Entry, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, fmt.Errorf("procs: list processes: %w", err)
	}
	out := make([]Entry, 0, len(kps))
	for _, kp := range kps {
		// A zombie has ended; it stays listed until its parent reaps it.
		if kp.Proc.P_stat == sZomb {
			continue
		}
		st := kp.Proc.P_starttime
		out = append(out, Entry{
			PID: int(kp.Proc.P_pid), PPID: int(kp.Eproc.Ppid), PGID: int(kp.Eproc.Pgid),
			Start: st.Sec*1e6 + int64(st.Usec),
		})
	}
	return out, nil
}
