// Command signal runs on until killed, as a container's main process does,
// or with "signal PID" reports whether it may signal process PID.
package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "signal" {
		pid, err := strconv.Atoi(os.Args[2])
		if err == nil {
			err = syscall.Kill(pid, 0)
		}
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("signalled")
		return
	}
	time.Sleep(time.Hour)
}
