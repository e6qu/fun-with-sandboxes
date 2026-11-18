//go:build darwin

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
	"github.com/e6qu/fun-with-sandboxes/internal/pathmap"
)

// memExecRole is the role in which the helper binary replaces itself with
// a program held to a memory limit: fws-memexec MiB path argv...
//
// The kernel keeps a jetsam limit across no fork and no exec; only a
// program started or executed with it gets it. The container library sets
// it on what a container's processes start, but the exec anchor and
// docker-init are Go programs, whose fork and exec the library does not
// reach: they start a container's program through this role, after Go has
// set its session, process group and terminal, so the program executes
// with the limit and as their child.
const memExecRole = "memexec"

func init() {
	if os.Getenv(RoleVar) == memExecRole {
		os.Exit(runMemExec(os.Args))
	}
}

func runMemExec(args []string) int {
	if len(args) < 4 {
		fmt.Fprintln(os.Stderr, "fws-memexec: usage: fws-memexec MiB PATH ARGV0 [ARG...]")
		return 127
	}
	mib, err := strconv.Atoi(args[1])
	if err != nil || mib <= 0 {
		fmt.Fprintf(os.Stderr, "fws-memexec: a memory limit of %q MiB\n", args[1])
		return 127
	}
	err = libsystem.ExecLimited(args[2], args[3:], withoutVars(os.Environ(), RoleVar), mib)
	fmt.Fprintf(os.Stderr, "exec %s: %v\n", args[2], err)
	return 127
}

// memoryLimited returns how to start path with argv and env so that it
// executes held to a memory limit of mib MiB: through this binary in the
// memexec role.
func memoryLimited(path string, argv, env []string, mib int) (string, []string, []string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", nil, nil, err
	}
	return self, append([]string{"fws-memexec", strconv.Itoa(mib), path}, argv...),
		append(withoutVars(env, RoleVar), RoleVar+"="+memExecRole), nil
}

// containerMemoryMiB is the memory limit in MiB the container's file holds
// (pathmap.MemoryFile), beside the pids limit's file env names, or 0.
func containerMemoryMiB(env []string) int {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, pathmap.PidsVar+"="); ok && v != "" {
			b, err := os.ReadFile(filepath.Join(filepath.Dir(v), pathmap.MemoryFile))
			if err != nil {
				return 0
			}
			mib, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil || mib < 0 {
				return 0
			}
			return mib
		}
	}
	return 0
}
