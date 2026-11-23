package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
)

// rawConnect connects to loopback port with the connect system call,
// which the container library does not see, as a program that skips it
// does: the sandbox alone decides.
func rawConnect(port string) error {
	p, err := strconv.Atoi(port)
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	sa := unix.RawSockaddrInet4{Len: unix.SizeofSockaddrInet4, Family: unix.AF_INET, Addr: [4]byte{127, 0, 0, 1}}
	sa.Port = uint16(p>>8) | uint16(p&0xff)<<8
	if _, _, e := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unix.SizeofSockaddrInet4); e != 0 {
		return e
	}
	return nil
}

// rawProcTable reads the size of the process table's kern.proc.all
// listing with the sysctl system call, which the library does not see.
func rawProcTable() error {
	mib := []int32{1, 14, 0} // CTL_KERN, KERN_PROC, KERN_PROC_ALL
	var size uintptr
	if _, _, e := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)), 0, uintptr(unsafe.Pointer(&size)), 0, 0); e != 0 {
		return e
	}
	if size == 0 {
		return errors.New("an empty process table")
	}
	return nil
}

// proc_pidinfo's flavors listing a process's threads and reading one
// (sys/proc_info.h), and pth_priority's offset in proc_threadinfo.
const (
	procPIDListThreads = 6
	procPIDThreadInfo  = 5
	pthPriorityOffset  = 40
)

// threadPriority prints the highest base priority of the process's
// threads, which a QoS clamp lowers: 4 under background's.
func threadPriority() error {
	buf := make([]byte, 8*256)
	n, err := libsystem.ProcPidinfo(os.Getpid(), procPIDListThreads, 0, buf)
	if err != nil {
		return err
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
	fmt.Println("priority", best)
	return nil
}
