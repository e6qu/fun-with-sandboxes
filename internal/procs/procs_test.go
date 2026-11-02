package procs

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseCPUTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:00.05":     50 * time.Millisecond,
		"12:34.50":    12*time.Minute + 34500*time.Millisecond,
		"01:02:03":    time.Hour + 2*time.Minute + 3*time.Second,
		"2-03:04:05":  2*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second,
		"00:00:00":    0,
		"1:02:03.10":  time.Hour + 2*time.Minute + 3100*time.Millisecond,
		"10-00:00:00": 240 * time.Hour,
	} {
		got, err := ParseCPUTime(in)
		if err != nil || got != want {
			t.Errorf("ParseCPUTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "12", "a:b", "x-1:00"} {
		if _, err := ParseCPUTime(bad); err == nil {
			t.Errorf("ParseCPUTime(%q) accepted", bad)
		}
	}
}

func TestParse(t *testing.T) {
	ps, err := Parse([]byte("    1     0     1  14000   1:23.45\n  420     1   420    512   0:00.01\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Proc{
		{PID: 1, PPID: 0, PGID: 1, RSS: 14000 * 1024, CPU: 83450 * time.Millisecond},
		{PID: 420, PPID: 1, PGID: 420, RSS: 512 * 1024, CPU: 10 * time.Millisecond},
	}
	if len(ps) != 2 || ps[0] != want[0] || ps[1] != want[1] {
		t.Fatalf("Parse = %+v", ps)
	}
}

func TestParseTop(t *testing.T) {
	out := []byte("  UID   PID  PPID   C STIME   TTY           TIME CMD\n" +
		"  501   100     1   0  9:00AM ??         0:00.10 /toolbox serve --flag x\n" +
		"  501   200     1   0  9:00AM ??         0:00.10 /other\n")
	titles, rows, err := ParseTop(out, map[int]bool{100: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 8 || len(rows) != 1 || rows[0][1] != "100" || rows[0][7] != "/toolbox serve --flag x" {
		t.Fatalf("ParseTop = %v %v", titles, rows)
	}
	if _, _, err := ParseTop([]byte("USER COMMAND\nme x\n"), nil); err != ErrNoPIDColumn {
		t.Fatalf("missing PID column: %v", err)
	}
}

func TestListSeesThisProcess(t *testing.T) {
	ps, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.PID == os.Getpid() {
			if p.PGID != syscall.Getpgrp() || p.RSS == 0 {
				t.Fatalf("this process reads as %+v", p)
			}
			return
		}
	}
	t.Fatalf("this process (%d) is missing from %d processes", os.Getpid(), len(ps))
}

func TestParseProcargs(t *testing.T) {
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, 2)
	b = append(b, "/bin/app\x00\x00\x00\x00"...)
	b = append(b, "app\x00-v\x00"...)
	b = append(b, "PATH=/bin\x00SANDBOX_ROOT=/r\x00\x00\x00"...)
	env, err := parseProcargs(b)
	if err != nil || len(env) != 2 || env[0] != "PATH=/bin" || env[1] != "SANDBOX_ROOT=/r" {
		t.Fatalf("parseProcargs = %q, %v", env, err)
	}
}

// A process keeps its tag when it starts a session of its own, so Tagged
// finds it and KillTagged ends it, leaving untagged processes alone.
func TestKillTaggedEndsProcessesOutsideTheGroup(t *testing.T) {
	tag := "SANDBOX_ROOT=/procs-test/" + strconv.Itoa(os.Getpid())
	start := func(env ...string) *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		cmd.Env = append(os.Environ(), env...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	tagged, other := start(tag), start()
	defer other.Process.Kill()
	reaped := make(chan error, 1)
	go func() { reaped <- tagged.Wait() }()

	pids, err := Tagged(tag)
	if err != nil || len(pids) != 1 || pids[0] != tagged.Process.Pid {
		t.Fatalf("Tagged = %v, %v; want [%d]", pids, err, tagged.Process.Pid)
	}
	if err := KillTagged(tag); err != nil {
		t.Fatal(err)
	}
	if err := <-reaped; err == nil {
		t.Fatal("the tagged process exited cleanly instead of being killed")
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("KillTagged ended an untagged process: %v", err)
	}
}

func TestTopArgsSelectTheContainersProcesses(t *testing.T) {
	pids := map[int]bool{30: true, 7: true}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-ef"}, "-ef"},
		{[]string{"aux"}, "aux -p 7,30"},
		{[]string{"-o", "pid"}, "-o pid -p 7,30"},
		{[]string{"-o", "pid,user", "-A"}, "-o pid,user -A"},
		{[]string{"-l"}, "-l -p 7,30"},
	} {
		if got := strings.Join(TopArgs(c.args, pids), " "); got != c.want {
			t.Errorf("TopArgs(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestOwnListsThisProcess(t *testing.T) {
	es, err := Own()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.PID == os.Getpid() {
			if e.PPID != os.Getppid() || e.PGID != syscall.Getpgrp() || e.Start == 0 {
				t.Fatalf("this process listed as %+v; want ppid %d, pgid %d and a start time", e, os.Getppid(), syscall.Getpgrp())
			}
			return
		}
	}
	t.Fatal("Own does not list this process")
}

// WaitExit returns once a process that is not this one's child exits.
func TestWaitExitSeesAnotherProcessExit(t *testing.T) {
	// The shell's child is the shell's to reap, not this process's.
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & echo $!; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var pid int
	if _, err := fmt.Fscan(out, &pid); err != nil {
		t.Fatal(err)
	}
	if left, err := WaitExit([]int{pid}, 50*time.Millisecond); err != nil || len(left) != 1 {
		t.Fatalf("WaitExit on a running process = %v, %v; want it still running", left, err)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	if left, err := WaitExit([]int{pid}, 10*time.Second); err != nil || len(left) != 0 {
		t.Fatalf("WaitExit after SIGKILL = %v, %v", left, err)
	}
	if left, err := WaitExit([]int{pid, 1 << 30}, time.Second); err != nil || len(left) != 0 {
		t.Fatalf("WaitExit on gone processes = %v, %v", left, err)
	}
}
