//go:build darwin

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func init() {
	if os.Getenv(RoleVar) == "" && filepath.Base(os.Args[0]) == filepath.Base(InitPath) {
		os.Exit(runInit(os.Args[1:]))
	}
}

// runInit runs the command as tini, Docker's init, does: as the
// container's pid 1 it passes the signals it receives on to the command,
// reaps every process that ends up its child, and exits as the command
// does. On a terminal the command leads its own foreground process group,
// so the terminal's signals reach it once.
func runInit(args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "docker-init: usage: docker-init [--] PROGRAM [ARGS...]")
		return 1
	}
	signals := make(chan os.Signal, 32)
	signal.Notify(signals)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if _, err := unix.IoctlGetTermios(0, unix.TIOCGETA); err == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: 0}
		signal.Ignore(syscall.SIGTTOU, syscall.SIGTTIN)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "docker-init: exec %s failed: %v\n", args[0], err)
		return 127
	}
	child := cmd.Process.Pid
	for s := range signals {
		sig := s.(syscall.Signal)
		if sig != syscall.SIGCHLD {
			if sig != syscall.SIGTTOU && sig != syscall.SIGTTIN && sig != syscall.SIGURG {
				syscall.Kill(child, sig)
			}
			continue
		}
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			if pid == child {
				if ws.Signaled() {
					return 128 + int(ws.Signal())
				}
				return ws.ExitStatus()
			}
		}
	}
	return 1
}
