package procs

import (
	"errors"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// WaitExit waits until each of pids has exited, or timeout passes, and
// returns those still running, through a pidfd for each.
func WaitExit(pids []int, timeout time.Duration) ([]int, error) {
	fds := map[int]int{}
	defer func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}()
	for _, pid := range pids {
		fd, err := unix.PidfdOpen(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		fds[pid] = fd
	}
	deadline := time.Now().Add(timeout)
	for len(fds) > 0 {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		var polls []unix.PollFd
		var order []int
		for pid, fd := range fds {
			polls = append(polls, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
			order = append(order, pid)
		}
		_, err := unix.Poll(polls, int(left/time.Millisecond)+1)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for i, p := range polls {
			if p.Revents != 0 {
				unix.Close(fds[order[i]])
				delete(fds, order[i])
			}
		}
	}
	out := make([]int, 0, len(fds))
	for pid := range fds {
		out = append(out, pid)
	}
	return out, nil
}
