// Command limits prints the limits it runs under: its descriptor limit, its
// scheduling priority, its disk I/O policy, and whether it has a controlling
// terminal.
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
	fmt.Printf("nofile=%d/%d nice=%d iopolicy=%d tty=%v\n", nofile.Cur, nofile.Max, nice, policy, ttyErr == nil)
}
