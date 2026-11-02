package procs

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

func ownPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("procs: list processes: %w", err)
	}
	uid := uint32(os.Getuid())
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fi, err := os.Stat("/proc/" + e.Name())
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == uid {
			out = append(out, pid)
		}
	}
	return out, nil
}

// Environ returns the environment pid started with, from
// /proc/<pid>/environ.
func Environ(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, err
	}
	var env []string
	for _, kv := range bytes.Split(b, []byte{0}) {
		if len(kv) > 0 {
			env = append(env, string(kv))
		}
	}
	return env, nil
}
