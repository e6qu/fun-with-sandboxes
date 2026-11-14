// Command limits prints the limits it runs under: its descriptor limit, its
// scheduling priority, its disk I/O policy, whether it has a controlling
// terminal, and its process limit.
package main

import (
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
	fmt.Printf("nofile=%d/%d nice=%d iopolicy=%d tty=%v nproc=%d\n", nofile.Cur, nofile.Max, nice, policy, ttyErr == nil, nproc.Cur)
}

// rlimitNproc is RLIMIT_NPROC on macOS.
const rlimitNproc = 7
