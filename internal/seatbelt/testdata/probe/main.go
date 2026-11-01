// Command probe attempts one operation and exits 0 if it succeeded, 1 if the
// operating system refused it.
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(op, arg string) error {
	switch op {
	case "write":
		return os.WriteFile(arg, []byte("x"), 0o600)
	case "read":
		_, err := os.ReadFile(arg)
		return err
	case "list":
		_, err := os.ReadDir(arg)
		return err
	case "bind":
		l, err := net.Listen("tcp", "127.0.0.1:"+arg)
		if err != nil {
			return err
		}
		return l.Close()
	case "connect":
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+arg, 2*time.Second)
		if err != nil {
			return err
		}
		return c.Close()
	case "signal":
		pid, err := strconv.Atoi(arg)
		if err != nil {
			return err
		}
		return syscall.Kill(pid, 0)
	case "exec":
		return syscall.Exec(arg, []string{arg}, nil)
	}
	return fmt.Errorf("unknown op %q", op)
}
