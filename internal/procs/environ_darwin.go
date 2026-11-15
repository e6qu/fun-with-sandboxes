package procs

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Environ returns the environment pid started with, from the kernel's copy
// of its arguments, kern.procargs2.
func Environ(pid int) ([]string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return parseProcargs(b)
}

// A zombie's p_stat, and the p_flag of a process working on its exit
// (sys/proc.h).
const (
	sZomb  = 5
	pWexit = 0x00002000
)

// Own lists the user's live processes from one kern.proc.uid sysctl, without
// starting ps.
func Own() ([]Entry, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, fmt.Errorf("procs: list processes: %w", err)
	}
	out := make([]Entry, 0, len(kps))
	for i := range kps {
		if e, ok := entry(&kps[i]); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// Find is pid's entry in the process table, if it lives, from one
// kern.proc.pid sysctl.
func Find(pid int) (Entry, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return Entry{}, false
	}
	return entry(kp)
}

// entry is a live process's entry. A zombie has ended; it stays listed
// until its parent reaps it. A process working on its exit has ended too,
// and has already reported NOTE_EXIT.
func entry(kp *unix.KinfoProc) (Entry, bool) {
	if kp.Proc.P_stat == sZomb || kp.Proc.P_flag&pWexit != 0 {
		return Entry{}, false
	}
	st := kp.Proc.P_starttime
	return Entry{
		PID: int(kp.Proc.P_pid), PPID: int(kp.Eproc.Ppid), PGID: int(kp.Eproc.Pgid),
		Start: st.Sec*1e6 + int64(st.Usec),
	}, true
}
