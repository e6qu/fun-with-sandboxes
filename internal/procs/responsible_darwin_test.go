package procs

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
)

// A process that disclaimed its responsibility, as a container's main
// process does, answers for what its descendants start even once one has
// started a session of its own and executed a program with a cleared
// environment, as a daemon does: Responsible finds it, KillResponsible
// ends it, and a process that answers to another is left alone.
func TestKillResponsibleEndsADaemonizedDescendant(t *testing.T) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	daemonize := `exit if fork; POSIX::setsid(); exit if fork; print "$$\n"; close STDOUT; exec "/bin/sleep", "60"`
	script := "/usr/bin/env -i /usr/bin/perl -MPOSIX -e '" + daemonize + "'; read x"
	pid, err := libsystem.Spawn("/bin/sh", []string{"sh", "-c", script}, os.Environ(), libsystem.SpawnAttr{
		Files: []uintptr{stdinR.Fd(), outW.Fd(), 2}, Disclaim: true,
	})
	stdinR.Close()
	outW.Close()
	if err != nil {
		t.Fatal(err)
	}
	main, ok := Find(pid)
	if !ok {
		t.Fatalf("the main process %d is not listed", pid)
	}
	line, err := bufio.NewReader(outR).ReadString('\n')
	if err != nil {
		t.Fatalf("the daemon's pid: %v", err)
	}
	daemon, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(daemon, syscall.SIGKILL)

	other := exec.Command("sleep", "60")
	other.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer other.Process.Kill()

	// The main process ends, as a container's does before the shim cleans
	// up after it.
	stdinW.Close()
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		t.Fatal(err)
	}
	if env, err := Environ(daemon); err != nil || len(env) != 0 {
		t.Logf("the daemon's environment: %v %v", env, err)
	}
	pids, err := Responsible(main)
	if err != nil || len(pids) != 1 || pids[0] != daemon {
		t.Fatalf("Responsible = %v, %v; want [%d]", pids, err, daemon)
	}
	if err := KillResponsible(main); err != nil {
		t.Fatal(err)
	}
	if _, ok := Find(daemon); ok {
		t.Fatal("the daemon outlived KillResponsible")
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("KillResponsible ended a process answering to another: %v", err)
	}
}

// A process started before the main process, or after a later process
// took its pid, does not answer to it, whatever macOS reports.
func TestAnswersGuardsAgainstReusedPIDs(t *testing.T) {
	main := Entry{PID: os.Getpid(), Start: 100}
	for _, c := range []struct {
		p      Entry
		reused int64
	}{
		{Entry{PID: main.PID, Start: 100}, 0},
		{Entry{PID: 1, Start: 50}, 0},
		{Entry{PID: 1, Start: 300}, 200},
	} {
		if Answers(c.p, main, c.reused) {
			t.Errorf("Answers(%+v, reused %d) = true", c.p, c.reused)
		}
	}
}
