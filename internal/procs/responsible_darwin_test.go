package procs

import (
	"bufio"
	"fmt"
	"io"
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
// environment, as a daemon does: Responsible finds it while the main
// process lives, KillEntries ends it once the main process has ended, and a
// process that answers to another is left alone.
func TestKillEntriesEndsADaemonizedDescendant(t *testing.T) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// The main process is this test's binary, as a container's is the
	// engine's spawn helper until it executes the command.
	env := append(os.Environ(), daemonHelperVar+"=main")
	pid, err := libsystem.Spawn(os.Args[0], []string{os.Args[0]}, env, libsystem.SpawnAttr{
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

	// While the main process lives, the daemon answers to it.
	pids, err := Responsible(main)
	if err != nil || len(pids) != 1 || pids[0] != daemon {
		t.Fatalf("Responsible = %v, %v; want [%d] (the daemon answers to %d)", pids, err, daemon, libsystem.ResponsiblePID(daemon))
	}
	d, ok := Find(daemon)
	if !ok {
		t.Fatalf("the daemon %d is not listed", daemon)
	}
	// The main process ends, as a container's does before the shim cleans
	// up after it; the shim kills what it found answering to it.
	stdinW.Close()
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := KillEntries([]Entry{d}); err != nil {
		t.Fatal(err)
	}
	if _, ok := Find(daemon); ok {
		t.Fatal("the daemon outlived KillEntries")
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("KillEntries ended a process answering to another: %v", err)
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

const daemonHelperVar = "FWS_PROCS_TEST_DAEMON"

// The test's binary, run with daemonHelperVar set, plays a container's
// processes: main starts a launcher in a session of its own with a cleared
// environment, which starts a daemon and exits, so launchd becomes the
// daemon's parent; main then waits for its standard input to close.
func init() {
	switch os.Getenv(daemonHelperVar) {
	case "main":
		self, err := os.Executable()
		if err != nil {
			os.Exit(1)
		}
		launcher, err := os.StartProcess(self, []string{self}, &os.ProcAttr{
			Env: []string{daemonHelperVar + "=launch"}, Files: []*os.File{nil, os.Stdout, os.Stderr},
			Sys: &syscall.SysProcAttr{Setsid: true},
		})
		if err != nil {
			os.Exit(1)
		}
		if st, err := launcher.Wait(); err != nil || !st.Success() {
			os.Exit(1)
		}
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	case "launch":
		p, err := os.StartProcess("/bin/sleep", []string{"sleep", "60"}, &os.ProcAttr{
			Env: []string{}, Files: []*os.File{nil, nil, nil},
		})
		if err != nil {
			os.Exit(1)
		}
		fmt.Println(p.Pid)
		os.Exit(0)
	}
}
