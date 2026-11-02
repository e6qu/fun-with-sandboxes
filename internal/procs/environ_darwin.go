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
