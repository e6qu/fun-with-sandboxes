package shim

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
	"github.com/e6qu/fun-with-sandboxes/internal/netstat"
	"github.com/e6qu/fun-with-sandboxes/internal/procs"
)

// The enforcer's cadence: the CPU period, as the kernel's default CFS
// period, at which it also samples memory, and the rescan interval that
// finds processes nothing told it about, such as an exec's.
const (
	cpuPeriod    = 100 * time.Millisecond
	rescanPeriod = time.Second
)

// enforcer knows the container's processes and holds them to its limits
// without cgroups: it sums their physical footprints and kills the largest
// past the memory limit, as the kernel's OOM killer picks one in a cgroup;
// it stops and continues them to keep their CPU time within the quota; and
// it kills processes forked past the pids limit.
type enforcer struct {
	tag  string
	main int

	mu       sync.Mutex
	res      Resources
	paused   bool
	stopped  bool
	resumeAt time.Time
	debt     time.Duration
	members  map[int]*member
	// others are processes found not to be the container's, by start
	// time, so each is read once.
	others map[int]int64

	// goneRead and goneWritten are the disk I/O of members that ended.
	goneRead, goneWritten uint64
	// net counts the members' network traffic.
	net *netstat.Watcher

	periods, throttledPeriods, oomKills uint64
	throttledTime                       time.Duration
	oom                                 bool

	kq      int
	running bool
	wake    chan struct{}
	quit    chan struct{}
	done    chan struct{}
}

type member struct {
	start int64
	cpu   uint64
	// read and written are the bytes of disk I/O last sampled.
	read, written uint64
}

func newEnforcer(tag string, main int, r Resources, net *netstat.Watcher) *enforcer {
	e := &enforcer{
		tag: tag, main: main, res: r, kq: -1, net: net,
		members: map[int]*member{}, others: map[int]int64{},
		wake: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{}),
	}
	e.mu.Lock()
	e.scanLocked()
	e.startLocked()
	e.mu.Unlock()
	return e
}

// startLocked starts the enforcing loop once there is a limit to enforce.
func (e *enforcer) startLocked() {
	if e.running || !e.res.any() {
		return
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return
	}
	e.kq = kq
	for pid := range e.members {
		e.watchLocked(pid)
	}
	e.running = true
	go e.loop()
}

// watchLocked asks the kernel to report the process's forks, so a fork
// bomb is seen at once and not at the next rescan.
func (e *enforcer) watchLocked(pid int) {
	if e.kq < 0 {
		return
	}
	ev := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_FORK | unix.NOTE_EXEC}
	unix.Kevent(e.kq, []unix.Kevent_t{ev}, nil, nil)
}

func (e *enforcer) close() {
	e.mu.Lock()
	running := e.running
	e.mu.Unlock()
	if running {
		close(e.quit)
		<-e.done
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.kq >= 0 {
		unix.Close(e.kq)
		e.kq = -1
	}
	if e.stopped && !e.paused {
		e.signalLocked(syscall.SIGCONT)
	}
}

func (e *enforcer) loop() {
	defer close(e.done)
	events := make([]unix.Kevent_t, 16)
	lastScan := time.Now()
	last := time.Now()
	for {
		timeout := unix.NsecToTimespec(int64(cpuPeriod))
		n, _ := unix.Kevent(e.kq, nil, events, &timeout)
		select {
		case <-e.quit:
			return
		default:
		}
		e.mu.Lock()
		now := time.Now()
		forked := n > 0
		select {
		case <-e.wake:
			forked = true
		default:
		}
		for _, ev := range events[:max(n, 0)] {
			if _, ok := e.members[int(ev.Ident)]; ok {
				e.watchLocked(int(ev.Ident))
			}
		}
		if forked || now.Sub(lastScan) >= rescanPeriod {
			e.scanLocked()
			lastScan = now
		}
		if now.Sub(last) >= cpuPeriod {
			e.tickLocked(now, now.Sub(last))
			last = now
		}
		e.mu.Unlock()
	}
}

// scanLocked updates the member set from the process table: the main
// process's group, the descendants of members, and any process that
// started with the container's tag.
func (e *enforcer) scanLocked() {
	es, err := procs.Own()
	if err != nil {
		return
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Start < es[j].Start })
	live := map[int]procs.Entry{}
	for _, p := range es {
		live[p.PID] = p
	}
	for pid, m := range e.members {
		if p, ok := live[pid]; !ok || p.Start != m.start {
			e.goneRead += m.read
			e.goneWritten += m.written
			delete(e.members, pid)
		}
	}
	for pid, start := range e.others {
		if p, ok := live[pid]; !ok || p.Start != start {
			delete(e.others, pid)
		}
	}
	var added []int
	for _, p := range es {
		if _, ok := e.members[p.PID]; ok {
			continue
		}
		if start, ok := e.others[p.PID]; ok && start == p.Start {
			continue
		}
		_, parent := e.members[p.PPID]
		if p.PGID == e.main || parent || e.tagged(p.PID) {
			e.members[p.PID] = &member{start: p.Start}
			added = append(added, p.PID)
			e.watchLocked(p.PID)
		} else {
			e.others[p.PID] = p.Start
		}
	}
	if (e.stopped || e.paused) && len(added) > 0 {
		for _, pid := range added {
			syscall.Kill(pid, syscall.SIGSTOP)
		}
	}
	e.limitPidsLocked()
	pids := make([]int, 0, len(e.members))
	for pid := range e.members {
		pids = append(pids, pid)
	}
	e.net.SetMembers(pids)
}

func (e *enforcer) tagged(pid int) bool {
	env, err := procs.Environ(pid)
	if err != nil {
		return false
	}
	for _, kv := range env {
		if kv == e.tag {
			return true
		}
	}
	return false
}

// limitPidsLocked kills the newest processes past the pids limit: the
// kernel cannot be made to fail their fork, so they end right after it.
func (e *enforcer) limitPidsLocked() {
	limit := e.res.PidsLimit
	if limit <= 0 || int64(len(e.members)) <= limit {
		return
	}
	pids := make([]int, 0, len(e.members))
	for pid := range e.members {
		if pid != e.main {
			pids = append(pids, pid)
		}
	}
	sort.Slice(pids, func(i, j int) bool { return e.members[pids[i]].start > e.members[pids[j]].start })
	for _, pid := range pids {
		if int64(len(e.members)) <= limit {
			break
		}
		syscall.Kill(pid, syscall.SIGKILL)
		delete(e.members, pid)
	}
}

// tickLocked samples the members once a period: their footprint for the
// memory limit and their CPU time for the quota.
func (e *enforcer) tickLocked(now time.Time, elapsed time.Duration) {
	var used time.Duration
	var total uint64
	largest, largestPID := uint64(0), 0
	for pid, m := range e.members {
		u, err := libsystem.ProcUsage(pid)
		if err != nil {
			continue
		}
		if u.CPU > m.cpu {
			used += time.Duration(u.CPU - m.cpu)
		}
		m.cpu, m.read, m.written = u.CPU, u.DiskRead, u.DiskWritten
		total += u.Footprint
		if u.Footprint > largest {
			largest, largestPID = u.Footprint, pid
		}
	}
	if limit := e.res.Memory; limit > 0 && total > uint64(limit) && largestPID != 0 {
		syscall.Kill(largestPID, syscall.SIGKILL)
		e.oom = true
		e.oomKills++
	}
	if e.res.NanoCPUs <= 0 {
		if e.stopped {
			e.stopped = false
			if !e.paused {
				e.signalLocked(syscall.SIGCONT)
			}
		}
		return
	}
	e.periods++
	rate := float64(e.res.NanoCPUs) / 1e9
	if e.stopped {
		e.throttledPeriods++
		e.throttledTime += elapsed
		if !now.Before(e.resumeAt) {
			e.stopped, e.debt = false, 0
			if !e.paused {
				e.signalLocked(syscall.SIGCONT)
			}
		}
		return
	}
	// The members owe the CPU time they used beyond the quota; at most one
	// period's unused quota carries over.
	e.debt += used - time.Duration(rate*float64(elapsed))
	if floor := -time.Duration(rate * float64(cpuPeriod)); e.debt < floor {
		e.debt = floor
	}
	if e.debt > 0 && !e.paused {
		// Stopped, the members earn the quota back at rate.
		pause := time.Duration(float64(e.debt) / rate)
		e.stopped, e.resumeAt = true, now.Add(pause)
		e.signalLocked(syscall.SIGSTOP)
	}
}

func (e *enforcer) signalLocked(sig syscall.Signal) {
	for pid := range e.members {
		syscall.Kill(pid, sig)
	}
}

func (e *enforcer) pause(on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.paused == on {
		return nil
	}
	e.scanLocked()
	e.paused = on
	if on {
		e.signalLocked(syscall.SIGSTOP)
	} else if !e.stopped {
		e.signalLocked(syscall.SIGCONT)
	}
	return nil
}

func (e *enforcer) update(r Resources) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.res = r
	e.limitPidsLocked()
	e.startLocked()
}

// renice sets the members' scheduling priority. Only root may lower a nice
// value, so a request for a higher priority than the processes have fails.
func (e *enforcer) renice(nice int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scanLocked()
	var errs []string
	for pid := range e.members {
		if err := syscall.Setpriority(syscall.PRIO_PROCESS, pid, nice); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Sprintf("process %d: %v", pid, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("set the scheduling priority to %d (only root raises a priority): %s", nice, strings.Join(errs, "; "))
	}
	return nil
}

func (e *enforcer) track() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		select {
		case e.wake <- struct{}{}:
		default:
		}
		return
	}
	e.scanLocked()
}

func (e *enforcer) state() (State, error) {
	st := e.usage()
	net, err := e.net.Total()
	if err != nil {
		return State{}, err
	}
	st.Net = net
	return st, nil
}

func (e *enforcer) usage() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		e.scanLocked()
	}
	st := State{
		Pid: e.main, Shim: os.Getpid(), Periods: e.periods, ThrottledPeriods: e.throttledPeriods,
		ThrottledTime: e.throttledTime, OOMKills: e.oomKills, Resources: e.res,
	}
	st.DiskRead, st.DiskWritten = e.goneRead, e.goneWritten
	for pid, m := range e.members {
		st.Members = append(st.Members, pid)
		if u, err := libsystem.ProcUsage(pid); err == nil {
			st.Footprint += u.Footprint
			m.read, m.written = u.DiskRead, u.DiskWritten
		}
		st.DiskRead += m.read
		st.DiskWritten += m.written
	}
	sort.Ints(st.Members)
	return st
}

func (e *enforcer) oomKilled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.oom
}
