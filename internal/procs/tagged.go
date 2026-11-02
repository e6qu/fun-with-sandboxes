package procs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Tagged lists this user's processes whose environment, as it was when they
// started, holds tag, such as SANDBOX_ROOT=<a container's root>. A container
// process keeps its tag when it leaves its process group or session, and a
// call to unsetenv leaves the environment it started with as it was, so
// only one that executes a program with a cleared environment sheds it.
func Tagged(tag string) ([]int, error) {
	pids, err := ownPIDs()
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var out []int
	for _, pid := range pids {
		if pid == self {
			continue
		}
		env, err := Environ(pid)
		if err != nil {
			// The process ended, or turned into one the user cannot read,
			// since the listing.
			continue
		}
		for _, kv := range env {
			if kv == tag {
				out = append(out, pid)
				break
			}
		}
	}
	return out, nil
}

// KillTagged kills every process Tagged finds and returns once none is
// left, killing what they started meanwhile too.
func KillTagged(tag string) error {
	deadline := time.Now().Add(killTimeout)
	for {
		pids, err := Tagged(tag)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("procs: processes %v holding %s outlived SIGKILL", pids, tag)
		}
		for _, pid := range pids {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("procs: kill %d: %w", pid, err)
			}
		}
		// A killed process leaves within milliseconds; macOS notifies only
		// a parent, which the engine is not of these.
		for _, pid := range pids {
			for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
				time.Sleep(exitProbe)
			}
		}
	}
}

const (
	killTimeout = 10 * time.Second
	exitProbe   = 2 * time.Millisecond
)

// parseProcargs reads the environment out of a kern.procargs2 buffer: the
// argument count, the executable's path and its NUL padding, the arguments,
// then the environment, each NUL-ended.
func parseProcargs(b []byte) ([]string, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("procs: procargs of %d bytes", len(b))
	}
	argc := int(binary.LittleEndian.Uint32(b))
	b = b[4:]
	// The executable path, then the NULs padding it.
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return nil, fmt.Errorf("procs: procargs without an executable path")
	}
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	var env []string
	for n := 0; len(b) > 0; n++ {
		i := bytes.IndexByte(b, 0)
		if i <= 0 {
			break
		}
		if n >= argc {
			env = append(env, string(b[:i]))
		}
		b = b[i+1:]
	}
	return env, nil
}
