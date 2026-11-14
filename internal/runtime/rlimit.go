package runtime

import "strconv"

// Resource limit numbers, as macOS's sys/resource.h defines them.
const (
	RlimitCPU     = 0
	RlimitFsize   = 1
	RlimitData    = 2
	RlimitStack   = 3
	RlimitCore    = 4
	RlimitAS      = 5
	RlimitMemlock = 6
	RlimitNofile  = 8
	rlimitNproc   = 7
)

// ForkCeiling is the process limit a container's processes get where the
// user may run max processes: max less a reserve of an eighth of it, and at
// least 64, which the user's own programs keep.
func ForkCeiling(max uint64) uint64 {
	reserve := max / 8
	if reserve < 64 {
		reserve = 64
	}
	if max <= reserve {
		return max
	}
	return max - reserve
}

var rlimitNames = map[int]string{
	RlimitCPU: "cpu", RlimitFsize: "fsize", RlimitData: "data", RlimitStack: "stack",
	RlimitCore: "core", RlimitAS: "as", RlimitMemlock: "memlock", RlimitNofile: "nofile",
}

// RlimitName is the name docker run --ulimit uses for a resource.
func RlimitName(r int) string {
	if n, ok := rlimitNames[r]; ok {
		return n
	}
	return "resource " + strconv.Itoa(r)
}
