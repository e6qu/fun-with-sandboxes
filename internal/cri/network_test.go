package cri

import (
	"fmt"
	"testing"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestPodSubnets(t *testing.T) {
	for in, want := range map[string]string{
		"10.244.1.0/24":              "[10.244.1.0/24]",
		"10.244.1.0/24,fd00:1::/64":  "[10.244.1.0/24 fd00:1::/64]",
		"fd00:1::/64, 10.244.2.0/24": "[10.244.2.0/24 fd00:1::/64]",
		"fd00:10:244:1::/64":         "[fd00:10:244:1::/64]",
	} {
		got, err := podSubnets(in)
		if err != nil || fmt.Sprint(got) != want {
			t.Errorf("podSubnets(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "10.244.1.5/24", "fd00:1::5/64", "not a cidr", "10.244.1.0/24,10.244.2.0/24", "fd00:1::/64,fd00:2::/64"} {
		if got, err := podSubnets(in); err == nil {
			t.Errorf("podSubnets(%q) = %v, want an error", in, got)
		}
	}
}

// A Linux container's resources become the engine's limits: the memory
// limit, the CPU quota over its period as a share of CPUs, the shares.
func TestLinuxLimits(t *testing.T) {
	r := linuxLimits(&runtimeapi.LinuxContainerResources{MemoryLimitInBytes: 1 << 28, CpuPeriod: 100000, CpuQuota: 250000, CpuShares: 256})
	if r.Memory != 1<<28 || r.NanoCPUs != 2500000000 || r.CPUShares != 256 {
		t.Errorf("limits: %+v", r)
	}
	if r := linuxLimits(&runtimeapi.LinuxContainerResources{CpuQuota: -1, CpuPeriod: 100000}); r.NanoCPUs != 0 {
		t.Errorf("an unlimited quota became %d nano CPUs", r.NanoCPUs)
	}
	if r := linuxLimits(nil); r.Memory != 0 || r.NanoCPUs != 0 || r.CPUShares != 0 {
		t.Errorf("no resources became %+v", r)
	}
}
