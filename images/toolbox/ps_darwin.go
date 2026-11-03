package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ps lists the process table as ps reads it, a pid and a name a line.
func ps() error {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return err
	}
	for _, kp := range kps {
		fmt.Printf("%d %s\n", kp.Proc.P_pid, unix.ByteSliceToString(kp.Proc.P_comm[:]))
	}
	return nil
}
