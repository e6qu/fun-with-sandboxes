package engine

import (
	"context"
	"runtime"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/procs"
)

// statsInterval is dockerd's collection cadence.
const statsInterval = time.Second

// groups returns the process groups belonging to the container: the main
// process's and each running exec's.
func (e *Engine) groups(c *Container) map[int]bool {
	g := map[int]bool{}
	c.mu.Lock()
	if c.task != nil {
		g[c.task.Pid()] = true
	}
	c.mu.Unlock()
	for _, x := range e.execsFor(c.rec.ID) {
		x.mu.Lock()
		if x.running && x.pid != 0 {
			g[x.pid] = true
		}
		x.mu.Unlock()
	}
	return g
}

// members are the container's processes: those of its main process's and
// its execs' process groups, and those that left them, which still name its
// root in their environment.
func (e *Engine) members(c *Container) ([]procs.Proc, error) {
	g := e.groups(c)
	if len(g) == 0 {
		return nil, nil
	}
	tagged, err := procs.Tagged(c.tag())
	if err != nil {
		return nil, err
	}
	escaped := map[int]bool{}
	for _, pid := range tagged {
		escaped[pid] = true
	}
	all, err := procs.List()
	if err != nil {
		return nil, err
	}
	var out []procs.Proc
	for _, p := range all {
		if g[p.PGID] || escaped[p.PID] {
			out = append(out, p)
		}
	}
	return out, nil
}

// Top lists the container's processes as ps shows them, keeping the rows of
// its process groups, as dockerd filters ps output by container PID.
func (e *Engine) Top(ref, psArgs string) (container.TopResponse, error) {
	c, err := e.Container(ref)
	if err != nil {
		return container.TopResponse{}, err
	}
	if !c.isRunning() {
		return container.TopResponse{}, conflict("container %s is not running", c.rec.ID)
	}
	if psArgs == "" {
		psArgs = "-ef"
	}
	members, err := e.members(c)
	if err != nil {
		return container.TopResponse{}, err
	}
	pids := map[int]bool{}
	for _, p := range members {
		pids[p.PID] = true
	}
	titles, rows, err := procs.Top(strings.Fields(psArgs), pids)
	if err != nil {
		return container.TopResponse{}, invalid("%v", err)
	}
	return container.TopResponse{Titles: titles, Processes: rows}, nil
}

func (e *Engine) sample(c *Container) (container.StatsResponse, error) {
	members, err := e.members(c)
	if err != nil {
		return container.StatsResponse{}, err
	}
	var cpu time.Duration
	var rss uint64
	for _, p := range members {
		cpu += p.CPU
		rss += p.RSS
	}
	ncpu := uint64(runtime.NumCPU())
	memLimit := memTotal()
	c.mu.Lock()
	r, task := c.rec.HostConfig.Resources, c.task
	c.mu.Unlock()
	if r.Memory > 0 {
		memLimit = uint64(r.Memory)
	}
	var pidsLimit uint64
	if r.PidsLimit != nil && *r.PidsLimit > 0 {
		pidsLimit = uint64(*r.PidsLimit)
	}
	var throttling container.ThrottlingData
	var blkio container.BlkioStats
	if task != nil {
		if st, err := task.State(); err == nil {
			throttling = container.ThrottlingData{
				Periods: st.Periods, ThrottledPeriods: st.ThrottledPeriods, ThrottledTime: uint64(st.ThrottledTime),
			}
			// Report the measure the memory limit holds.
			rss = st.Footprint
			blkio = blkioStats(c.root(), st.DiskRead, st.DiskWritten)
		}
	}
	return container.StatsResponse{
		ID: c.rec.ID, Name: "/" + c.Name(), OSType: runtime.GOOS, Read: time.Now(),
		CPUStats: container.CPUStats{
			CPUUsage: container.CPUUsage{TotalUsage: uint64(cpu)},
			// Machine CPU time elapsed: with it the client's CPU% is the
			// share of one CPU the container used, as Docker reports.
			SystemUsage:    uint64(time.Since(e.started)) * ncpu,
			OnlineCPUs:     uint32(ncpu),
			ThrottlingData: throttling,
		},
		MemoryStats: container.MemoryStats{Usage: rss, Limit: memLimit},
		PidsStats:   container.PidsStats{Current: uint64(len(members)), Limit: pidsLimit},
		BlkioStats:  blkio,
	}, nil
}

// Stats sends resource samples. With stream it sends one per collection
// interval until the container stops or ctx ends; without, it sends one
// sample with the previous interval's CPU usage, as dockerd does, or with
// oneShot one sample at once.
func (e *Engine) Stats(ctx context.Context, ref string, stream, oneShot bool, fn func(container.StatsResponse) error) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	if !c.isRunning() {
		return fn(container.StatsResponse{ID: c.rec.ID, Name: "/" + c.Name(), OSType: runtime.GOOS, Read: time.Now()})
	}
	c.mu.Lock()
	done := c.runDone
	c.mu.Unlock()
	prev, err := e.sample(c)
	if err != nil {
		return err
	}
	if !stream && oneShot {
		return fn(prev)
	}
	if stream {
		if err := fn(prev); err != nil {
			return err
		}
	}
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-done:
			return nil
		case <-t.C:
		}
		cur, err := e.sample(c)
		if err != nil {
			return err
		}
		cur.PreRead, cur.PreCPUStats = prev.Read, prev.CPUStats
		if err := fn(cur); err != nil || !stream {
			return err
		}
		prev = cur
	}
}

// blkioStats reports the container's disk I/O against the device its root
// lives on, as a cgroup v2 host reports io_service_bytes_recursive.
func blkioStats(root string, read, written uint64) container.BlkioStats {
	var st unix.Stat_t
	if err := unix.Stat(root, &st); err != nil {
		return container.BlkioStats{}
	}
	major, minor := uint64(unix.Major(uint64(st.Dev))), uint64(unix.Minor(uint64(st.Dev)))
	return container.BlkioStats{IoServiceBytesRecursive: []container.BlkioStatEntry{
		{Major: major, Minor: minor, Op: "read", Value: read},
		{Major: major, Minor: minor, Op: "write", Value: written},
	}}
}
