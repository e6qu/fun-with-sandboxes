// Command forker prints a line, starts a child that inherits its stdout and
// outlives it, and exits once the child runs.
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		fmt.Println("child", os.Getpid())
		// Tell the parent this process runs, then hold stdout open.
		ready := os.NewFile(3, "ready")
		ready.Close()
		time.Sleep(time.Hour)
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// A container's argv[0] is the path it names in the container, which
	// the host need not hold; the executable's own path the host does.
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	p, err := os.StartProcess(self, []string{self, "child"}, &os.ProcAttr{Files: []*os.File{nil, os.Stdout, os.Stderr, w}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	w.Close()
	fmt.Println("parent", p.Pid)
	// The read returns once the child closed its end of the pipe.
	r.Read(make([]byte, 1))
	os.Exit(3)
}
