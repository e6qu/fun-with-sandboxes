package netstat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const controlName = "com.apple.network.statistics"

// sysprotoControl is SYSPROTO_CONTROL from sys/sys_domain.h, which
// golang.org/x/sys does not name.
const sysprotoControl = 2

// Watcher holds a connection to the kernel's network statistics and keeps
// an Account current from it. The kernel announces each socket as it opens,
// and the watcher asks for its report at once, while the process that opened
// it most likely still runs to be told apart; it stops watching any socket
// not the container's. The kernel reports each watched socket's final
// counts as it closes, and the latest of the others on each poll.
type Watcher struct {
	*Account
	fd int

	// poll serializes polls: the kernel runs one at a time per connection.
	poll    sync.Mutex
	mu      sync.Mutex
	context uint64
	replies map[uint64]chan message
	err     error
	done    chan struct{}
}

// Watch connects to the kernel's network statistics for the TCP and UDP
// sockets.
func Watch(a *Account) (*Watcher, error) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, sysprotoControl)
	if err != nil {
		return nil, fmt.Errorf("network statistics: %w", err)
	}
	info := unix.CtlInfo{}
	copy(info.Name[:], controlName)
	if err := unix.IoctlCtlInfo(fd, &info); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("network statistics: look up %s: %w", controlName, err)
	}
	if err := unix.Connect(fd, &unix.SockaddrCtl{ID: info.Id}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("network statistics: connect: %w", err)
	}
	// Room for the reports of every socket closing between two reads.
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 1<<20)
	w := &Watcher{Account: a, fd: fd, replies: map[uint64]chan message{}, done: make(chan struct{})}
	go w.read()
	for _, p := range []uint32{providerTCP, providerUDP} {
		if err := w.addAll(p); err != nil {
			w.Close()
			return nil, err
		}
	}
	// A first poll sorts the sockets open already, and has the kernel
	// report a socket's final counts as it closes.
	if _, err := w.Total(); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

func (w *Watcher) Close() error {
	err := unix.Shutdown(w.fd, unix.SHUT_RDWR)
	<-w.done
	unix.Close(w.fd)
	return err
}

func (w *Watcher) read() {
	defer close(w.done)
	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(w.fd, buf)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || n == 0 {
			w.fail(errors.New("network statistics: the kernel closed the connection"))
			return
		}
		msgs, err := parse(buf[:n])
		for _, m := range msgs {
			w.handle(m)
		}
		if err != nil {
			w.fail(err)
			return
		}
	}
}

func (w *Watcher) handle(m message) {
	switch m.typ {
	case msgSrcAdded:
		w.send(msgGetUpdate, m.ref)
	case msgSrcUpdate:
		if m.src != nil && !w.update(*m.src, m.flags&flagClosing != 0) && m.flags&flagClosing == 0 {
			w.send(msgRemSrc, m.ref)
		}
	case msgSrcRemoved:
		w.remove(m.ref)
	case msgSuccess, msgError:
		w.mu.Lock()
		ch := w.replies[m.context]
		delete(w.replies, m.context)
		w.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// send asks something of one source, an answer to be handled as a report.
func (w *Watcher) send(typ uint32, ref uint64) {
	msg := make([]byte, 24)
	binary.LittleEndian.PutUint32(msg[8:], typ)
	binary.LittleEndian.PutUint16(msg[12:], uint16(len(msg)))
	binary.LittleEndian.PutUint64(msg[16:], ref)
	unix.Write(w.fd, msg)
}

func (w *Watcher) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
	for c, ch := range w.replies {
		close(ch)
		delete(w.replies, c)
	}
}

func (w *Watcher) nextContext() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.context++
	return w.context
}

// request sends a message under ctx and waits for the kernel's answer to
// it, every report it sends before that answer handled.
func (w *Watcher) request(msg []byte, ctx uint64) (message, error) {
	w.mu.Lock()
	if w.err != nil {
		w.mu.Unlock()
		return message{}, w.err
	}
	ch := make(chan message, 1)
	w.replies[ctx] = ch
	w.mu.Unlock()
	binary.LittleEndian.PutUint64(msg, ctx)
	binary.LittleEndian.PutUint16(msg[12:], uint16(len(msg)))
	if _, err := unix.Write(w.fd, msg); err != nil {
		w.mu.Lock()
		delete(w.replies, ctx)
		w.mu.Unlock()
		return message{}, fmt.Errorf("network statistics: %w", err)
	}
	m, ok := <-ch
	if !ok {
		w.mu.Lock()
		defer w.mu.Unlock()
		return message{}, w.err
	}
	if m.typ == msgError {
		return m, fmt.Errorf("network statistics: %w", syscall.Errno(m.errno))
	}
	return m, nil
}

func (w *Watcher) addAll(provider uint32) error {
	msg := make([]byte, 56)
	binary.LittleEndian.PutUint32(msg[8:], msgAddAllSrcs)
	binary.LittleEndian.PutUint32(msg[32:], provider)
	if _, err := w.request(msg, w.nextContext()); err != nil {
		return fmt.Errorf("watch provider %d: %w", provider, err)
	}
	return nil
}

// Total polls every socket's counts and returns the container's traffic.
// The kernel answers a poll in fragments, each asking for the next under
// the same context.
func (w *Watcher) Total() (Counts, error) {
	w.poll.Lock()
	defer w.poll.Unlock()
	ctx := w.nextContext()
	for {
		msg := make([]byte, 24)
		binary.LittleEndian.PutUint32(msg[8:], msgGetUpdate)
		binary.LittleEndian.PutUint16(msg[14:], flagContinuation)
		binary.LittleEndian.PutUint64(msg[16:], srcRefAll)
		m, err := w.request(msg, ctx)
		if err != nil {
			return Counts{}, err
		}
		if m.flags&flagContinuation == 0 {
			return w.total(), nil
		}
	}
}
