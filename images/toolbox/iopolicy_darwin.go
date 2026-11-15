package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
)

// ioPolicy prints the process's disk I/O policy tier, "iopolicy N"; with
// watch, it prints "self=N child=M" for its own tier and that of a child it
// starts, every time either changes, until it is killed.
func ioPolicy(watch bool) error {
	self, err := libsystem.IOPolicy()
	if err != nil {
		return err
	}
	if !watch {
		fmt.Println("iopolicy", self)
		return nil
	}
	last := ""
	for {
		if self, err = libsystem.IOPolicy(); err != nil {
			return err
		}
		out, err := exec.Command(os.Args[0], "iopolicy").Output()
		if err != nil {
			return err
		}
		line := fmt.Sprintf("self=%d child=%s", self, strings.TrimPrefix(strings.TrimSpace(string(out)), "iopolicy "))
		if line != last {
			fmt.Println(line)
			last = line
		}
		time.Sleep(200 * time.Millisecond)
	}
}
