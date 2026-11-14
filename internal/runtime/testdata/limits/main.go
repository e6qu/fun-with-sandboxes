// Command limits prints the limits it runs under: its descriptor limit, its
// scheduling priority, its disk I/O policy, whether it has a controlling
// terminal, its process limit, and its threads' highest base priority,
// which a QoS clamp lowers.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
)

func main() {
	var nofile syscall.Rlimit
	syscall.Getrlimit(syscall.RLIMIT_NOFILE, &nofile)
	nice, _ := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	policy, _ := libsystem.IOPolicy()
	_, ttyErr := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	var nproc syscall.Rlimit
	syscall.Getrlimit(rlimitNproc, &nproc)
	fmt.Printf("nofile=%d/%d nice=%d iopolicy=%d tty=%v nproc=%d priority=%d\n", nofile.Cur, nofile.Max, nice, policy, ttyErr == nil, nproc.Cur, priority())
}

// proc_pidinfo's flavors listing a process's threads and reading one
// (sys/proc_info.h), and pth_priority's offset in proc_threadinfo.
const (
	procPIDListThreads = 6
	procPIDThreadInfo  = 5
	pthPriorityOffset  = 40
)

// priority is the highest base priority of this process's threads.
func priority() int {
	buf := make([]byte, 8*256)
	n, err := libsystem.ProcPidinfo(os.Getpid(), procPIDListThreads, 0, buf)
	if err != nil {
		return -1
	}
	best := -1
	info := make([]byte, 160)
	for i := 0; i+8 <= n; i += 8 {
		th := binary.LittleEndian.Uint64(buf[i:])
		if _, err := libsystem.ProcPidinfo(os.Getpid(), procPIDThreadInfo, th, info); err != nil {
			continue
		}
		best = max(best, int(int32(binary.LittleEndian.Uint32(info[pthPriorityOffset:]))))
	}
	return best
}

// rlimitNproc is RLIMIT_NPROC on macOS.
const rlimitNproc = 7
