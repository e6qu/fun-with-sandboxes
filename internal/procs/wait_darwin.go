package procs

import (
	"errors"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// WaitExit waits until each of pids has exited, or timeout passes, and
// returns those still running. kqueue reports any process of the user
// exiting, not only a child's.
func WaitExit(pids []int, timeout time.Duration) ([]int, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	defer unix.Close(kq)
	changes := make([]unix.Kevent_t, len(pids))
	for i, pid := range pids {
		changes[i] = unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT | unix.EV_RECEIPT, Fflags: unix.NOTE_EXIT}
	}
	// With EV_RECEIPT each change reports its own result: ESRCH for a
	// process already gone.
	receipts := make([]unix.Kevent_t, len(pids))
	n, err := unix.Kevent(kq, changes, receipts, nil)
	if err != nil {
		return nil, err
	}
	running := map[int]bool{}
	for _, r := range receipts[:n] {
		switch errno := syscall.Errno(r.Data); {
		case r.Flags&unix.EV_ERROR == 0, errno == 0:
			running[int(r.Ident)] = true
		case errno == syscall.ESRCH:
		default:
			return nil, errno
		}
	}
	deadline := time.Now().Add(timeout)
	events := make([]unix.Kevent_t, len(pids))
	for len(running) > 0 {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		ts := unix.NsecToTimespec(int64(left))
		n, err := unix.Kevent(kq, nil, events, &ts)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, ev := range events[:n] {
			delete(running, int(ev.Ident))
		}
	}
	out := make([]int, 0, len(running))
	for pid := range running {
		out = append(out, pid)
	}
	return out, nil
}
