package engine

import (
	"math"
	goruntime "runtime"
	"strings"

	"github.com/moby/moby/api/types/container"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
	"github.com/e6qu/fun-with-sandboxes/internal/shim"
)

const (
	// minMemory is Docker's smallest memory limit.
	minMemory = 6 << 20
	// defaultCPUPeriod is the CFS period Docker assumes when a quota comes
	// without one, in microseconds.
	defaultCPUPeriod = 100000
	// rlimInfinity is macOS's RLIM_INFINITY.
	rlimInfinity = math.MaxInt64
)

// ulimitResources are the --ulimit names macOS enforces per process. nproc
// counts every process of the engine's user, not the container's; data and
// as are refused by macOS's setrlimit; the rest have no macOS limit.
var ulimitResources = map[string]int{
	"nofile": runtime.RlimitNofile, "fsize": runtime.RlimitFsize, "cpu": runtime.RlimitCPU,
	"core": runtime.RlimitCore, "stack": runtime.RlimitStack,
}

// containerLimits turns a container's resource settings into what the
// spawn helper applies to each of its processes and what its shim
// enforces, refusing what neither can.
func containerLimits(r container.Resources) (runtime.Limits, shim.Resources, error) {
	var l runtime.Limits
	var s shim.Resources
	if r.Memory != 0 {
		if r.Memory < minMemory {
			return l, s, invalid("Minimum memory limit allowed is 6MB")
		}
		s.Memory = r.Memory
	}
	switch {
	case r.MemorySwap == 0, r.MemorySwap == -1:
	case r.Memory == 0:
		return l, s, invalid("You should always set the Memory limit when using Memoryswap limit")
	case r.MemorySwap < r.Memory:
		return l, s, invalid("Minimum memoryswap limit should be larger than memory limit, see usage")
	case r.MemorySwap != r.Memory:
		return l, s, unsupported("a swap limit apart from the memory limit is not supported: the memory limit already counts a process's compressed and swapped-out memory")
	}
	if r.MemoryReservation != 0 {
		return l, s, unsupported("memory reservations are not supported: macOS reclaims memory from processes by its own measure")
	}
	if r.MemorySwappiness != nil && *r.MemorySwappiness != -1 {
		return l, s, unsupported("memory swappiness is not supported: macOS swaps by its own measure")
	}
	if r.OomKillDisable != nil && *r.OomKillDisable {
		return l, s, unsupported("disabling the OOM killer is not supported: the memory limit is enforced only by ending processes")
	}

	ncpu := int64(goruntime.NumCPU())
	switch {
	case r.NanoCPUs != 0 && (r.CPUQuota != 0 || r.CPUPeriod != 0):
		return l, s, invalid("Conflicting options: Nano CPUs and CPU Period/Quota cannot both be set")
	case r.NanoCPUs < 0 || r.NanoCPUs > ncpu*1e9:
		return l, s, invalid("Range of CPUs is from 0.01 to %d.00, as there are only %d CPUs available", ncpu, ncpu)
	case r.NanoCPUs > 0:
		s.NanoCPUs = r.NanoCPUs
	case r.CPUQuota > 0:
		period := r.CPUPeriod
		if period == 0 {
			period = defaultCPUPeriod
		}
		if period < 1000 || period > 1000000 {
			return l, s, invalid("CPU cfs period can not be less than 1ms (i.e. 1000) or larger than 1 second (i.e. 1000000)")
		}
		if r.CPUQuota < 1000 {
			return l, s, invalid("CPU cfs quota can not be less than 1ms (i.e. 1000)")
		}
		s.NanoCPUs = min(r.CPUQuota*1e9/period, ncpu*1e9)
	}
	if r.CPUShares != 0 {
		if r.CPUShares < 2 {
			return l, s, invalid("CPU shares must be at least 2")
		}
		l.Nice = niceFor(r.CPUShares)
	}
	for _, k := range []struct {
		set  bool
		what string
	}{
		{r.CpusetCpus != "" || r.CpusetMems != "", "cpusets are not supported: macOS gives no process a choice of CPUs"},
		{r.CPURealtimePeriod != 0 || r.CPURealtimeRuntime != 0, "real-time CPU scheduling is not supported: it needs root"},
		{r.CPUCount != 0 || r.CPUPercent != 0 || r.IOMaximumIOps != 0 || r.IOMaximumBandwidth != 0, "Windows resource limits are not supported"},
		{len(r.BlkioWeightDevice) > 0 || len(r.BlkioDeviceReadBps) > 0 || len(r.BlkioDeviceWriteBps) > 0 ||
			len(r.BlkioDeviceReadIOps) > 0 || len(r.BlkioDeviceWriteIOps) > 0, "per-device block I/O limits are not supported: macOS sets disk I/O policy per process, not per device"},
		{len(r.Devices) > 0 || len(r.DeviceRequests) > 0 || len(r.DeviceCgroupRules) > 0, "devices are not supported: this engine runs without root"},
		{r.CgroupParent != "", "cgroup parents are not supported: macOS has no cgroups"},
	} {
		if k.set {
			return l, s, unsupported("%s", k.what)
		}
	}
	if r.BlkioWeight != 0 {
		if r.BlkioWeight < 10 || r.BlkioWeight > 1000 {
			return l, s, invalid("Range of blkio weight is from 10 to 1000")
		}
		l.IOPolicy = ioPolicyFor(r.BlkioWeight)
	}
	if r.PidsLimit != nil && *r.PidsLimit > 0 {
		s.PidsLimit = *r.PidsLimit
	}
	for _, u := range r.Ulimits {
		if u == nil {
			continue
		}
		res, ok := ulimitResources[strings.ToLower(u.Name)]
		if !ok {
			switch strings.ToLower(u.Name) {
			case "nproc":
				return l, s, unsupported("ulimit nproc is not supported: macOS counts it across every process of the engine's user")
			case "data", "as":
				return l, s, unsupported("ulimit %s is not supported: macOS does not enforce it", u.Name)
			case "memlock", "rss", "locks", "sigpending", "msgqueue", "nice", "rtprio", "rttime":
				return l, s, unsupported("ulimit %s is not supported: macOS has no such limit", u.Name)
			}
			return l, s, invalid("invalid ulimit type: %s", u.Name)
		}
		soft, hard := rlim(u.Soft), rlim(u.Hard)
		if soft > hard {
			return l, s, invalid("ulimit soft limit must be less than or equal to hard limit: %d > %d", u.Soft, u.Hard)
		}
		l.Rlimits = append(l.Rlimits, runtime.Rlimit{Resource: res, Cur: soft, Max: hard})
	}
	return l, s, nil
}

func rlim(v int64) uint64 {
	if v < 0 {
		return rlimInfinity
	}
	return uint64(v)
}

// niceFor maps CPU shares to a nice value: the default 1024 shares runs at
// nice 0, and each halving of the shares adds 3, about what halves a
// process's share of a contended CPU under macOS's scheduler. An
// unprivileged process cannot run below nice 0, so more shares than the
// default run at 0.
func niceFor(shares int64) int {
	if shares >= 1024 {
		return 0
	}
	return min(int(math.Round(3*math.Log2(1024/float64(shares)))), 19)
}

// ioPolicyFor maps a block I/O weight to macOS's disk I/O policy tiers:
// the upper half of the range keeps the standard policy, the next band
// the utility one, and the lowest weights throttle.
func ioPolicyFor(weight uint16) int {
	switch {
	case weight >= 500:
		return libsystem.IOPolicyStandard
	case weight >= 100:
		return libsystem.IOPolicyUtility
	}
	return libsystem.IOPolicyThrottle
}
