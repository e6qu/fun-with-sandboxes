package shim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/roles"
	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
)

// Files the shim keeps in the container's directory.
const (
	ConfigFile = "shim.json"
	LogFile    = "shim.log"
)

// ErrPortInUse is the error Launch returns when a published host port is
// taken.
var ErrPortInUse = errors.New("port is already allocated")

// Client talks to a container's shim.
type Client struct {
	socket string
	pid    int
}

// Launch writes cfg to dir, starts the shim, and returns once the
// container's main process runs, with the host ports its bindings took.
func Launch(dir string, cfg Config) (*Client, Ready, error) {
	path := filepath.Join(dir, ConfigFile)
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, Ready{}, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, Ready{}, err
	}
	os.Remove(cfg.ExitPath)
	self, err := roles.Binary()
	if err != nil {
		return nil, Ready{}, err
	}
	logf, err := os.OpenFile(filepath.Join(dir, LogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, Ready{}, err
	}
	defer logf.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return nil, Ready{}, err
	}
	defer r.Close()
	cmd := exec.Command(self, "shim", path)
	cmd.Env = append(withoutRole(os.Environ()), runtime.RoleVar+"="+Role)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.ExtraFiles = []*os.File{w}
	// The shim leads a session of its own, so a signal to the engine's
	// terminal or process group does not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		w.Close()
		return nil, Ready{}, err
	}
	w.Close()
	// The engine reaps the shim while it runs; a shim that outlives the
	// engine passes to launchd.
	go cmd.Wait()
	out, _ := io.ReadAll(r)
	var ready Ready
	if err := json.Unmarshal(bytes.TrimSpace(out), &ready); err != nil {
		tail, _ := os.ReadFile(filepath.Join(dir, LogFile))
		return nil, Ready{}, fmt.Errorf("container supervisor exited before the container ran: %s", lastLines(tail, 5))
	}
	if ready.Error != "" {
		if ready.PortInUse {
			return nil, ready, fmt.Errorf("%s: %w", ready.Error, ErrPortInUse)
		}
		return nil, ready, errors.New(ready.Error)
	}
	return &Client{socket: cfg.Socket, pid: ready.Pid}, ready, nil
}

// Connect reaches a running shim through its socket.
func Connect(socket string) (*Client, error) {
	c := &Client{socket: socket}
	st, err := c.State()
	if err != nil {
		return nil, err
	}
	c.pid = st.Pid
	return c, nil
}

func withoutRole(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, runtime.RoleVar+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return "no output"
	}
	return strings.Join(lines, "; ")
}

// open sends req on a new connection and reads the shim's first answer.
func (c *Client) open(req Request) (net.Conn, *bufio.Reader, Response, error) {
	conn, err := net.Dial("unix", c.socket)
	if err != nil {
		return nil, nil, Response{}, err
	}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		conn.Close()
		return nil, nil, Response{}, err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, nil, Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		conn.Close()
		return nil, nil, Response{}, err
	}
	if resp.Error != "" {
		conn.Close()
		return nil, nil, resp, errors.New(resp.Error)
	}
	return conn, r, resp, nil
}

func (c *Client) call(req Request) (Response, error) {
	conn, _, resp, err := c.open(req)
	if err != nil {
		return resp, err
	}
	conn.Close()
	return resp, nil
}

// Wait blocks until the container's main process has ended, or stop
// closes, and returns how it ended. An error means the shim went away
// without saying, or stop closed.
func (c *Client) Wait(stop <-chan struct{}) (Exit, error) {
	conn, err := net.Dial("unix", c.socket)
	if err != nil {
		return Exit{}, err
	}
	defer conn.Close()
	waited := make(chan struct{})
	defer close(waited)
	go func() {
		select {
		case <-stop:
			conn.Close()
		case <-waited:
		}
	}()
	b, _ := json.Marshal(Request{Op: "wait"})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return Exit{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return Exit{}, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Exit{}, err
	}
	if resp.Exit == nil {
		return Exit{}, errors.New("shim: no exit status")
	}
	return *resp.Exit, nil
}

func (c *Client) State() (State, error) {
	resp, err := c.call(Request{Op: "state"})
	if err != nil {
		return State{}, err
	}
	if resp.State == nil {
		return State{}, errors.New("shim: no state")
	}
	return *resp.State, nil
}

// Pid is the container's main process.
func (c *Client) Pid() int { return c.pid }

func (c *Client) Signal(sig syscall.Signal) error {
	_, err := c.call(Request{Op: "signal", Signal: int(sig)})
	return err
}

func (c *Client) Resize(h, w uint16) error {
	_, err := c.call(Request{Op: "resize", Height: h, Width: w})
	return err
}

func (c *Client) Pause() error {
	_, err := c.call(Request{Op: "pause"})
	return err
}

func (c *Client) Unpause() error {
	_, err := c.call(Request{Op: "unpause"})
	return err
}

// Update changes the limits the shim enforces and, with nice, the
// scheduling priority of the container's processes.
func (c *Client) Update(r Resources, nice *int) error {
	_, err := c.call(Request{Op: "update", Resources: &r, Nice: nice})
	return err
}

// Track asks the shim to look for new processes of the container now, such
// as an exec the engine just started.
func (c *Client) Track() error {
	_, err := c.call(Request{Op: "track"})
	return err
}

// Stdin returns a writer to the container's standard input. Closing it
// closes the container's input when closeInput is set.
func (c *Client) Stdin(closeInput bool) (io.WriteCloser, error) {
	conn, _, _, err := c.open(Request{Op: "stdin", Close: closeInput})
	if err != nil {
		return nil, err
	}
	return halfCloser{conn}, nil
}

type halfCloser struct{ net.Conn }

// Close ends the input and waits for the shim to have taken it, so input
// written before Close reaches the container first.
func (h halfCloser) Close() error {
	if uc, ok := h.Conn.(*net.UnixConn); ok {
		uc.CloseWrite()
		h.Conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		io.Copy(io.Discard, h.Conn)
	}
	return h.Conn.Close()
}

// Logs calls notify each time the shim appends to the container's log,
// until the shim exits or stop closes.
func (c *Client) Logs(stop <-chan struct{}, notify func()) error {
	conn, r, _, err := c.open(Request{Op: "logs"})
	if err != nil {
		return err
	}
	// The container may have written before the subscription began.
	notify()
	go func() {
		<-stop
		conn.Close()
	}()
	defer conn.Close()
	buf := make([]byte, 64)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			notify()
		}
		if err != nil {
			return nil
		}
	}
}

// ReadExit reads the status a shim wrote before it exited.
func ReadExit(path string) (Exit, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Exit{}, err
	}
	var e Exit
	err = json.Unmarshal(b, &e)
	return e, err
}
