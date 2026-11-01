// Command forker prints a line, starts a child that inherits its stdout and
// outlives it, and exits.
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		fmt.Println("child", os.Getpid())
		time.Sleep(time.Hour)
		return
	}
	p, err := os.StartProcess(os.Args[0], []string{os.Args[0], "child"}, &os.ProcAttr{Files: []*os.File{nil, os.Stdout, os.Stderr}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("parent", p.Pid)
	time.Sleep(200 * time.Millisecond)
	os.Exit(3)
}
