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

// Own lists the user's processes from /proc.
func Own() ([]Entry, error) {
	pids, err := ownPIDs()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(pids))
	for _, pid := range pids {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			continue
		}
		// The command, in parentheses, may hold spaces; the fields after
		// it are state, ppid, pgrp and, nineteenth, the start time.
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			continue
		}
		f := bytes.Fields(b[i+1:])
		if len(f) < 20 {
			continue
		}
		ppid, _ := strconv.Atoi(string(f[1]))
		pgid, _ := strconv.Atoi(string(f[2]))
		start, _ := strconv.ParseInt(string(f[19]), 10, 64)
		out = append(out, Entry{PID: pid, PPID: ppid, PGID: pgid, Start: start})
	}
	return out, nil
}
