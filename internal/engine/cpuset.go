package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// parseCpuset reads a cpuset, as Docker's --cpuset-cpus takes it ("0-3,6"),
// of a machine of n CPUs, and returns its CPUs in order.
func parseCpuset(s string, n int) ([]int, error) {
	bad := invalid("Invalid value %s for cpuset cpus", s)
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return nil, bad
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, bad
			}
		}
		for c := a; c <= b; c++ {
			if c >= n {
				return nil, invalid("Requested CPUs are not available - requested %s, available: %s", s, cpuRange(n))
			}
			seen[c] = true
		}
	}
	cpus := make([]int, 0, len(seen))
	for c := range seen {
		cpus = append(cpus, c)
	}
	sort.Ints(cpus)
	return cpus, nil
}

func cpuRange(n int) string {
	if n == 1 {
		return "0"
	}
	return fmt.Sprintf("0-%d", n-1)
}
