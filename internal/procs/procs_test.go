package procs

import (
	"os"
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
