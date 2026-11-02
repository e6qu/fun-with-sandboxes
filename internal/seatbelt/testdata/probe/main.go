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

	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
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
	case "socket":
		// arg names a socket domain and type.
		kinds := map[string][3]int{
			"unix":   {syscall.AF_UNIX, syscall.SOCK_STREAM, 0},
			"inet":   {syscall.AF_INET, syscall.SOCK_STREAM, 0},
			"inet6":  {syscall.AF_INET6, syscall.SOCK_DGRAM, 0},
			"route":  {syscall.AF_ROUTE, syscall.SOCK_RAW, 0},
			"system": {syscall.AF_SYSTEM, syscall.SOCK_DGRAM, 2},
			"ndrv":   {syscall.AF_NDRV, syscall.SOCK_RAW, 0},
			"icmp":   {syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP},
			"icmp6":  {syscall.AF_INET6, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMPV6},
		}
		k, ok := kinds[arg]
		if !ok {
			return fmt.Errorf("unknown socket kind %q", arg)
		}
		fd, err := syscall.Socket(k[0], k[1], k[2])
		if err != nil {
			return err
		}
		return syscall.Close(fd)
	case "shm":
		// Create and remove a POSIX shared memory object named arg.
		fd, err := libsystem.ShmOpen(arg, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		syscall.Close(fd)
		return libsystem.ShmUnlink(arg)
	case "sysvshm":
		// Create and remove a System V shared memory segment keyed arg.
		key, err := strconv.Atoi(arg)
		if err != nil {
			return err
		}
		id, err := unix.SysvShmGet(key, 4096, unix.IPC_CREAT|0o600)
		if err != nil {
			return err
		}
		_, err = unix.SysvShmCtl(id, unix.IPC_RMID, nil)
		return err
	case "sh":
		// Run a shell command, as images' scripts and popen do.
		return syscall.Exec("/bin/sh", []string{"sh", "-c", arg}, os.Environ())
	}
	return fmt.Errorf("unknown op %q", op)
}
