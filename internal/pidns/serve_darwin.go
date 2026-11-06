package pidns

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
	"github.com/e6qu/fun-with-sandboxes/internal/procs"
)

// Serve answers the requests of a container's library on l about the
// namespace whose file is file, until l closes.
func Serve(l net.Listener, file string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go handle(c, file)
	}
}

type request struct {
	Op, Pid, A, Size int32
	B                uint64
}

func handle(c net.Conn, file string) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	var b [RequestSize]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return
	}
	req := request{
		Op: int32(binary.NativeEndian.Uint32(b[0:])), Pid: int32(binary.NativeEndian.Uint32(b[4:])),
		A: int32(binary.NativeEndian.Uint32(b[8:])), Size: int32(binary.NativeEndian.Uint32(b[12:])),
		B: binary.NativeEndian.Uint64(b[16:]),
	}
	ret, data, err := answer(req, file)
	var errno syscall.Errno
	if err != nil {
		ret, data = -1, nil
		if !errors.As(err, &errno) {
			errno = syscall.EIO
		}
	}
	out := make([]byte, ReplySize, ReplySize+len(data))
	binary.NativeEndian.PutUint32(out, uint32(int32(ret)))
	binary.NativeEndian.PutUint32(out[4:], uint32(errno))
	c.Write(append(out, data...))
}

// members lists the namespace's live processes.
func members(file string) ([]procs.Entry, error) {
	tags, err := Tags(file)
	if err != nil {
		return nil, err
	}
	pids, err := procs.TaggedAny(tags)
	if err != nil {
		return nil, err
	}
	own, err := procs.Own()
	if err != nil {
		return nil, err
	}
	in := map[int]bool{}
	for _, p := range pids {
		in[p] = true
	}
	var out []procs.Entry
	for _, e := range own {
		if in[e.PID] {
			out = append(out, e)
		}
	}
	return out, nil
}

func answer(req request, file string) (int, []byte, error) {
	ms, err := members(file)
	if err != nil {
		return 0, nil, err
	}
	member := func(pid int32) bool {
		for _, m := range ms {
			if m.PID == int(pid) {
				return true
			}
		}
		return false
	}
	switch req.Op {
	case OpList:
		data := make([]byte, 4*len(ms))
		for i, m := range ms {
			binary.NativeEndian.PutUint32(data[4*i:], uint32(m.PID))
		}
		return len(ms), data, nil
	case OpKill:
		return 0, nil, kill(ms, req)
	}
	if !member(req.Pid) {
		return 0, nil, syscall.ESRCH
	}
	switch req.Op {
	case OpInfo:
		if req.Size < 0 || req.Size > MaxData {
			return 0, nil, syscall.EINVAL
		}
		buf := make([]byte, req.Size)
		n, err := libsystem.ProcPidinfo(int(req.Pid), int(req.A), req.B, buf)
		if err != nil {
			return 0, nil, err
		}
		return n, buf[:n], nil
	case OpPath:
		p, err := libsystem.ProcPidpath(int(req.Pid))
		if err != nil {
			return 0, nil, err
		}
		return len(p), []byte(p), nil
	case OpArgs:
		b, err := unix.SysctlRaw("kern.procargs2", int(req.Pid))
		if err != nil {
			return 0, nil, err
		}
		if len(b) > MaxData {
			return 0, nil, syscall.E2BIG
		}
		return len(b), b, nil
	}
	return 0, nil, syscall.EINVAL
}

// kill signals the namespace's processes req names, as kill(2) would in a
// PID namespace: ESRCH when it names none.
func kill(ms []procs.Entry, req request) error {
	sig := syscall.Signal(req.A)
	var targets []int
	for _, m := range ms {
		switch {
		case req.Pid > 0:
			if m.PID == int(req.Pid) {
				targets = append(targets, m.PID)
			}
		case req.Pid == -1:
			if m.PID != int(req.B) && m.PID != int(req.Size) {
				targets = append(targets, m.PID)
			}
		case req.Pid < -1:
			if m.PGID == -int(req.Pid) {
				targets = append(targets, m.PID)
			}
		}
	}
	if len(targets) == 0 {
		return syscall.ESRCH
	}
	var first error
	sent := false
	for _, p := range targets {
		if err := syscall.Kill(p, sig); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		sent = true
	}
	if sent {
		return nil
	}
	return first
}
