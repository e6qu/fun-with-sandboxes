// Package procs reads the host's process table through ps(1), as dockerd's
// `docker top` does, for the processes of one container.
package procs

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ps = "/bin/ps"

type Proc struct {
	PID, PPID, PGID int
	// RSS is the resident set size in bytes.
	RSS uint64
	// CPU is the accumulated user and system time.
	CPU time.Duration
}

// List returns every process on the host.
func List() ([]Proc, error) {
	out, err := exec.Command(ps, "-ax", "-o", "pid=,ppid=,pgid=,rss=,time=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return Parse(out)
}

// Parse reads `ps -o pid=,ppid=,pgid=,rss=,time=` output; ps reports RSS in
// kibibytes.
func Parse(out []byte) ([]Proc, error) {
	var procs []Proc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 5 {
			return nil, fmt.Errorf("ps: unexpected line %q", line)
		}
		var p Proc
		var err error
		ints := []*int{&p.PID, &p.PPID, &p.PGID}
		for i, dst := range ints {
			if *dst, err = strconv.Atoi(f[i]); err != nil {
				return nil, fmt.Errorf("ps: %q: %w", line, err)
			}
		}
		kib, err := strconv.ParseUint(f[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ps: %q: %w", line, err)
		}
		p.RSS = kib * 1024
		if p.CPU, err = ParseCPUTime(f[4]); err != nil {
			return nil, fmt.Errorf("ps: %q: %w", line, err)
		}
		procs = append(procs, p)
	}
	return procs, nil
}

// ParseCPUTime reads ps's accumulated time, [[dd-]hh:]mm:ss[.cc].
func ParseCPUTime(s string) (time.Duration, error) {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, fmt.Errorf("cpu time %q", s)
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("cpu time %q", s)
	}
	secs, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("cpu time %q", s)
	}
	total := time.Duration(secs * float64(time.Second))
	units := []time.Duration{time.Minute, time.Hour}
	for i := len(parts) - 2; i >= 0; i-- {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return 0, fmt.Errorf("cpu time %q", s)
		}
		total += time.Duration(n) * units[len(parts)-2-i]
	}
	return total + time.Duration(days)*24*time.Hour, nil
}

// Top runs ps with args and keeps the header and the rows whose PID column
// names one of pids.
func Top(args []string, pids map[int]bool) ([]string, [][]string, error) {
	cmd := exec.Command(ps, TopArgs(args, pids)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("ps %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return ParseTop(out, pids)
}

// TopArgs adds the container's pids to ps arguments that select no
// processes of their own, as dockerd adds them: without a selection ps lists
// the processes on its terminal, and the engine has none.
func TopArgs(args []string, pids map[int]bool) []string {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a[1:], "AaepUuxgt") && !optionArgument(args, a) {
			return args
		}
	}
	list := make([]int, 0, len(pids))
	for p := range pids {
		list = append(list, p)
	}
	if len(list) == 0 {
		return args
	}
	sort.Ints(list)
	parts := make([]string, len(list))
	for i, p := range list {
		parts[i] = strconv.Itoa(p)
	}
	return append(append([]string{}, args...), "-p", strings.Join(parts, ","))
}

// optionArgument reports whether a is the value of the option before it,
// such as the format after -o.
func optionArgument(args []string, a string) bool {
	for i := 1; i < len(args); i++ {
		if args[i] == a && (args[i-1] == "-o" || args[i-1] == "-O") {
			return true
		}
	}
	return false
}

var ErrNoPIDColumn = errors.New("couldn't find PID field in ps output")

// ParseTop splits ps output into titles and the rows of the given pids; the
// last column takes the rest of each line, since commands contain spaces.
func ParseTop(out []byte, pids map[int]bool) ([]string, [][]string, error) {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	titles := strings.Fields(lines[0])
	col := -1
	for i, t := range titles {
		if t == "PID" {
			col = i
		}
	}
	if col < 0 {
		return nil, nil, ErrNoPIDColumn
	}
	rows := [][]string{}
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) <= col {
			continue
		}
		pid, err := strconv.Atoi(f[col])
		if err != nil || !pids[pid] {
			continue
		}
		if len(f) > len(titles) {
			f = append(f[:len(titles)-1], strings.Join(f[len(titles)-1:], " "))
		}
		rows = append(rows, f)
	}
	return titles, rows, nil
}
