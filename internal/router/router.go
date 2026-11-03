// Package router carries connections to a container's ports that have no
// host port. The container library listens on such a port with a Unix
// socket in the container's port directory, which no other container can
// reach; a client sends the port it wants to the container's router, a
// loopback port every member of its stack may reach, and the router joins
// the connection to the socket.
package router

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// A request is the container port, two bytes big-endian; the answer is one
// byte, Connected or Refused, after which the connection carries the
// stream.
const (
	Connected = 0
	Refused   = 1
)

// SocketPath is where the container library listens on TCP port port.
func SocketPath(dir string, port int) string {
	return filepath.Join(dir, "tcp-"+strconv.Itoa(port))
}

// Router accepts on a loopback port and joins each connection to the
// container's listener on the port it names.
type Router struct {
	l   net.Listener
	dir string
	mu  sync.Mutex
	cs  map[net.Conn]struct{}
}

// Listen starts a router on addr for the port directory dir.
func Listen(addr, dir string) (*Router, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	r := &Router{l: l, dir: dir, cs: map[net.Conn]struct{}{}}
	go r.serve()
	return r, nil
}

func (r *Router) Addr() *net.TCPAddr { return r.l.Addr().(*net.TCPAddr) }

// Close stops accepting and ends every joined connection.
func (r *Router) Close() error {
	err := r.l.Close()
	r.mu.Lock()
	for c := range r.cs {
		c.Close()
	}
	r.mu.Unlock()
	return err
}

func (r *Router) track(c net.Conn, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		r.cs[c] = struct{}{}
	} else {
		delete(r.cs, c)
	}
}

func (r *Router) serve() {
	for {
		c, err := r.l.Accept()
		if err != nil {
			return
		}
		go r.join(c)
	}
}

func (r *Router) join(c net.Conn) {
	r.track(c, true)
	defer r.track(c, false)
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var req [2]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	up, err := net.Dial("unix", SocketPath(r.dir, int(binary.BigEndian.Uint16(req[:]))))
	if err != nil {
		c.Write([]byte{Refused})
		return
	}
	r.track(up, true)
	defer r.track(up, false)
	defer up.Close()
	if _, err := c.Write([]byte{Connected}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
	go func() { io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	type halfCloser interface{ CloseWrite() error }
	if h, ok := c.(halfCloser); ok {
		h.CloseWrite()
	}
}

// Dial reaches port of the container whose router listens on addr, as the
// container library does.
func Dial(addr string, port int) (net.Conn, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	var req [2]byte
	binary.BigEndian.PutUint16(req[:], uint16(port))
	var ans [1]byte
	if _, err := c.Write(req[:]); err == nil {
		_, err = io.ReadFull(c, ans[:])
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	if ans[0] != Connected {
		c.Close()
		return nil, errRefused
	}
	return c, nil
}

var errRefused = &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errors.New("connection refused"))}
