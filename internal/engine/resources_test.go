package engine

import (
	"fmt"
	"testing"

	"github.com/moby/moby/api/types/blkiodev"
	"github.com/moby/moby/api/types/container"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
)

func TestContainerLimits(t *testing.T) {
	pids := int64(50)
	l, s, err := containerLimits(container.Resources{
		Memory: 64 << 20, MemorySwap: 64 << 20, NanoCPUs: 5e8, CPUShares: 256, PidsLimit: &pids, BlkioWeight: 50,
		Ulimits: []*container.Ulimit{{Name: "nofile", Soft: 100, Hard: 200}, {Name: "core", Soft: -1, Hard: -1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Memory != 64<<20 || s.NanoCPUs != 5e8 || s.PidsLimit != 50 {
		t.Errorf("shim resources %+v", s)
	}
	if l.Nice != 6 || l.IOPolicy != libsystem.IOPolicyThrottle {
		t.Errorf("nice %d, I/O policy %d", l.Nice, l.IOPolicy)
	}
	want := []runtime.Rlimit{{Resource: runtime.RlimitNofile, Cur: 100, Max: 200}, {Resource: runtime.RlimitCore, Cur: rlimInfinity, Max: rlimInfinity}}
	if len(l.Rlimits) != 2 || l.Rlimits[0] != want[0] || l.Rlimits[1] != want[1] {
		t.Errorf("rlimits %+v, want %+v", l.Rlimits, want)
	}
}

func TestCPUQuotaBecomesACPURate(t *testing.T) {
	_, s, err := containerLimits(container.Resources{CPUQuota: 50000, CPUPeriod: 100000})
	if err != nil || s.NanoCPUs != 5e8 {
		t.Fatalf("quota 50ms per 100ms: %d CPUs, %v", s.NanoCPUs, err)
	}
	_, s, err = containerLimits(container.Resources{CPUQuota: 25000})
	if err != nil || s.NanoCPUs != 25e7 {
		t.Fatalf("quota 25ms per default period: %d CPUs, %v", s.NanoCPUs, err)
	}
}

// A cpuset holds the container to as many CPUs' time, and one of
// efficiency cores alone, numbered after the performance cores, to them
// through the background QoS clamp.
func TestCpusetBecomesACPURateAndPlacement(t *testing.T) {
	saved := cpuTopology
	defer func() { cpuTopology = saved }()
	cpuTopology = func() (int, int) { return 4, 6 }
	for _, c := range []struct {
		set   string
		cpus  int64
		clamp runtime.QoSClamp
	}{
		{"0,1", 2e9, runtime.QoSNone},
		{"4-5", 2e9, runtime.QoSBackground},
		{"3-5", 3e9, runtime.QoSNone},
		{"6,4,6", 2e9, runtime.QoSBackground},
	} {
		l, s, err := containerLimits(container.Resources{CpusetCpus: c.set, CpusetMems: "0"})
		if err != nil || s.NanoCPUs != c.cpus || l.QoSClamp != c.clamp {
			t.Errorf("cpuset %s: %d CPUs, clamp %d, %v; want %d, %d", c.set, s.NanoCPUs, l.QoSClamp, err, c.cpus, c.clamp)
		}
	}
	// A lower CPU rate holds beside the set.
	if _, s, err := containerLimits(container.Resources{CpusetCpus: "0-1", NanoCPUs: 5e8}); err != nil || s.NanoCPUs != 5e8 {
		t.Errorf("cpuset beside half a CPU: %d, %v", s.NanoCPUs, err)
	}
}

func TestParseCpuset(t *testing.T) {
	for in, want := range map[string]string{"0": "[0]", "0-2,5": "[0 1 2 5]", "3,1-2": "[1 2 3]"} {
		got, err := parseCpuset(in, 8)
		if err != nil || fmt.Sprint(got) != want {
			t.Errorf("parseCpuset(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "a", "-1", "2-1", "0-8", "1,,2"} {
		if _, err := parseCpuset(in, 8); err == nil {
			t.Errorf("parseCpuset(%q) accepted", in)
		}
	}
}

func TestNiceForShares(t *testing.T) {
	for shares, want := range map[int64]int{1024: 0, 4096: 0, 512: 3, 256: 6, 128: 9, 2: 19} {
		if got := niceFor(shares); got != want {
			t.Errorf("niceFor(%d) = %d, want %d", shares, got, want)
		}
	}
}

func TestContainerLimitsRefuse(t *testing.T) {
	perf, eff := cpuTopology()
	yes, swappy := true, int64(60)
	for name, c := range map[string]struct {
		r           container.Resources
		unsupported bool
	}{
		"memory below 6MB":       {container.Resources{Memory: 1 << 20}, false},
		"swap without memory":    {container.Resources{MemorySwap: 1 << 30}, false},
		"swap below memory":      {container.Resources{Memory: 1 << 30, MemorySwap: 1 << 29}, false},
		"swap beyond memory":     {container.Resources{Memory: 1 << 29, MemorySwap: 1 << 30}, true},
		"reservation":            {container.Resources{MemoryReservation: 1 << 29}, true},
		"swappiness":             {container.Resources{MemorySwappiness: &swappy}, true},
		"oom killer off":         {container.Resources{OomKillDisable: &yes}, true},
		"nano CPUs and quota":    {container.Resources{NanoCPUs: 1e9, CPUQuota: 50000}, false},
		"more CPUs than present": {container.Resources{NanoCPUs: int64(perf+eff+1) * 1e9}, false},
		"cpuset past the CPUs":   {container.Resources{CpusetCpus: "0-4096"}, false},
		"cpuset malformed":       {container.Resources{CpusetCpus: "3-1"}, false},
		"memory node past 0":     {container.Resources{CpusetMems: "1"}, false},
		"blkio out of range":     {container.Resources{BlkioWeight: 5}, false},
		"device read rate":       {container.Resources{BlkioDeviceReadBps: []*blkiodev.ThrottleDevice{{Path: "/dev/disk0", Rate: 1}}}, true},
		"ulimit nproc":           {container.Resources{Ulimits: []*container.Ulimit{{Name: "nproc", Soft: 1, Hard: 1}}}, true},
		"ulimit as":              {container.Resources{Ulimits: []*container.Ulimit{{Name: "as", Soft: 1, Hard: 1}}}, true},
		"ulimit unknown":         {container.Resources{Ulimits: []*container.Ulimit{{Name: "bogus", Soft: 1, Hard: 1}}}, false},
		"ulimit soft over hard":  {container.Resources{Ulimits: []*container.Ulimit{{Name: "nofile", Soft: 9, Hard: 1}}}, false},
	} {
		_, _, err := containerLimits(c.r)
		if err == nil || (KindOf(err) == KindUnsupported) != c.unsupported {
			t.Errorf("%s: %v, want unsupported %v", name, err, c.unsupported)
		}
	}
}

// The host's PID namespace and another container's are presented.
func TestPidModes(t *testing.T) {
	for mode, want := range map[container.PidMode]Kind{
		"":                  0,
		"host":              0,
		"container:db":      0,
		"bogus":             KindInvalid,
		"container:":        KindInvalid,
		"host:with-a-colon": KindInvalid,
	} {
		err := checkHostConfig(&container.HostConfig{PidMode: mode})
		if (err == nil) != (want == 0) || (err != nil && KindOf(err) != want) {
			t.Errorf("PID mode %q: %v", mode, err)
		}
	}
}

// A sysctl that already holds is accepted and sets nothing, as kind asks
// for IPv6 and forwarding on an IPv6 cluster's nodes; any other is
// refused, forwarding too for a container that is not privileged.
func TestSysctls(t *testing.T) {
	for _, c := range []struct {
		sysctls    map[string]string
		privileged bool
		ok         bool
	}{
		{map[string]string{"net.ipv6.conf.all.disable_ipv6": "0", "net.ipv6.conf.all.forwarding": "1"}, true, true},
		{map[string]string{"net.ipv6.conf.all.disable_ipv6": "0"}, false, true},
		{map[string]string{"net.ipv6.conf.all.forwarding": "1"}, false, false},
		{map[string]string{"net.ipv6.conf.all.disable_ipv6": "1"}, true, false},
		{map[string]string{"net.core.somaxconn": "1024"}, true, false},
	} {
		err := checkHostConfig(&container.HostConfig{Sysctls: c.sysctls, Privileged: c.privileged})
		if (err == nil) != c.ok {
			t.Errorf("sysctls %v, privileged %v: %v, want accepted %v", c.sysctls, c.privileged, err, c.ok)
		}
		if err != nil && KindOf(err) != KindUnsupported {
			t.Errorf("sysctls %v: %v, want an unsupported error", c.sysctls, err)
		}
	}
}
