package api

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
)

// Stream identifiers in the Docker multiplexed stream framing.
const (
	streamStdin  = 0
	streamStdout = 1
	streamStderr = 2
)

// muxWriter frames writes for one stream of a multiplexed connection: an
// 8-byte header (stream, three zero bytes, big-endian length) per chunk.
type muxWriter struct {
	mu     *sync.Mutex
	w      io.Writer
	stream byte
	flush  func()
}

func (m *muxWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var h [8]byte
	h[0] = m.stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(p)))
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.w.Write(h[:]); err != nil {
		return 0, err
	}
	n, err := m.w.Write(p)
	if m.flush != nil {
		m.flush()
	}
	return n, err
}

// rawWriter serialises writes on a TTY stream, which carries no framing.
type rawWriter struct {
	mu    *sync.Mutex
	w     io.Writer
	flush func()
}

func (r *rawWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.w.Write(p)
	if r.flush != nil {
		r.flush()
	}
	return n, err
}

type streams struct {
	stdout, stderr io.Writer
}

func newStreams(w io.Writer, tty bool, flush func()) streams {
	mu := &sync.Mutex{}
	if tty {
		r := &rawWriter{mu: mu, w: w, flush: flush}
		return streams{stdout: r, stderr: r}
	}
	return streams{
		stdout: &muxWriter{mu: mu, w: w, stream: streamStdout, flush: flush},
		stderr: &muxWriter{mu: mu, w: w, stream: streamStderr, flush: flush},
	}
}

func (s streams) of(stream string) io.Writer {
	if stream == "stderr" {
		return s.stderr
	}
	return s.stdout
}

// hijack takes over the connection for a raw stream, answering 101 when the
// client asked to upgrade, as Docker does.
func hijack(w http.ResponseWriter, r *http.Request, tty bool) (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("connection cannot be hijacked")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	ct := "application/vnd.docker.multiplexed-stream"
	if tty {
		ct = "application/vnd.docker.raw-stream"
	}
	if r.Header.Get("Upgrade") != "" {
		rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: " + ct + "\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	} else {
		rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: " + ct + "\r\n\r\n")
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, rw, nil
}

// closeWrite signals end of output to a client that keeps reading after it
// has sent its input.
func closeWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}
