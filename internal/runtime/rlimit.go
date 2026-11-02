package runtime

import "strconv"

// Resource limit numbers, as macOS's sys/resource.h defines them.
const (
	RlimitCPU    = 0
	RlimitFsize  = 1
	RlimitData   = 2
	RlimitStack  = 3
	RlimitCore   = 4
	RlimitAS     = 5
	RlimitNofile = 8
)

var rlimitNames = map[int]string{
	RlimitCPU: "cpu", RlimitFsize: "fsize", RlimitData: "data", RlimitStack: "stack",
	RlimitCore: "core", RlimitAS: "as", RlimitNofile: "nofile",
}

// RlimitName is the name docker run --ulimit uses for a resource.
func RlimitName(r int) string {
	if n, ok := rlimitNames[r]; ok {
		return n
	}
	return "resource " + strconv.Itoa(r)
}
