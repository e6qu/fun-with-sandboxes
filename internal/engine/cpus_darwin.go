package engine

import (
	goruntime "runtime"

	"golang.org/x/sys/unix"
)

// cpuTopology counts the Mac's performance and efficiency cores, as
// hw.perflevel0 and hw.perflevel1 do on Apple silicon; a Mac without
// performance levels has only performance cores.
var cpuTopology = func() (perf, eff int) {
	p, err := unix.SysctlUint32("hw.perflevel0.logicalcpu")
	if err != nil {
		return goruntime.NumCPU(), 0
	}
	e, _ := unix.SysctlUint32("hw.perflevel1.logicalcpu")
	return int(p), int(e)
}
