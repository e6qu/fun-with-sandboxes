package shim

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/e6qu/fun-with-sandboxes/internal/alloc"
	"github.com/e6qu/fun-with-sandboxes/internal/logfile"
	"github.com/e6qu/fun-with-sandboxes/internal/netstat"
	"github.com/e6qu/fun-with-sandboxes/internal/pidns"
	"github.com/e6qu/fun-with-sandboxes/internal/portproxy"
	"github.com/e6qu/fun-with-sandboxes/internal/procs"
	"github.com/e6qu/fun-with-sandboxes/internal/router"
	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
)

// outputDrain bounds how long the shim reads output after the main process
// ends, as os/exec's WaitDelay bounds it: a process that escaped the kill
// could hold the pipes open forever.
const outputDrain = 2 * time.Second

func init() {
	if os.Getenv(runtime.RoleVar) == Role && len(os.Args) > 1 {
		os.Exit(serve(os.Args[len(os.Args)-1]))
	}
}

type server struct {
	cfg  Config
	proc *runtime.Process
	enf  *enforcer

	mu      sync.Mutex
	stdin   io.WriteCloser
	master  *os.File
	logSubs map[chan struct{}]struct{}
	conns   map[net.Conn]struct{}
	exit    Exit
	exited  chan struct{}
	handled sync.WaitGroup
}

func serve(path string) int {
	ready := os.NewFile(3, "ready")
	report := func(r Ready) {
		json.NewEncoder(ready).Encode(r)
		ready.Close()
	}
	var cfg Config
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &cfg)
	}
	if err != nil {
		report(Ready{Error: fmt.Sprintf("read the supervisor's configuration: %v", err)})
		return 1
	}
	s := &server{cfg: cfg, logSubs: map[chan struct{}]struct{}{}, conns: map[net.Conn]struct{}{}, exited: make(chan struct{})}
	code, err := s.run(report)
	if err != nil {
		log.Printf("container %s: %v", cfg.ID, err)
	}
	return code
}

func (s *server) run(report func(Ready)) (int, error) {
	cfg := s.cfg
	var steps []string
	last := time.Now()
	mark := func(step string) {
		now := time.Now()
		steps = append(steps, step+" "+now.Sub(last).Round(time.Microsecond).String())
		last = now
	}
	os.Remove(cfg.Socket)
	l, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		report(Ready{Error: err.Error()})
		return 1, err
	}
	defer os.Remove(cfg.Socket)
	defer l.Close()
	if cfg.Procs != "" {
		os.Remove(cfg.Procs)
		pl, err := net.Listen("unix", cfg.Procs)
		if err != nil {
			report(Ready{Error: err.Error()})
			return 1, err
		}
		defer os.Remove(cfg.Procs)
		defer pl.Close()
		go pidns.Serve(pl, cfg.PidNamespace)
	}
	// The library asks from the container's first fork on; the count is
	// answered once the enforcer follows the container's processes, and a
	// connection waits in the listener's queue until then.
	var pidsListener net.Listener
	if cfg.Pids != "" {
		os.Remove(cfg.Pids)
		if pidsListener, err = net.Listen("unix", cfg.Pids); err != nil {
			report(Ready{Error: err.Error()})
			return 1, err
		}
		defer os.Remove(cfg.Pids)
		defer pidsListener.Close()
	}
	logs, err := logfile.Open(cfg.LogPath)
	if err != nil {
		report(Ready{Error: err.Error()})
		return 1, err
	}
	defer logs.Close()
	logs.Notify = s.notifyLogs
	mark("listen")

	// Watch the kernel's socket counts before the container's first process
	// opens a socket.
	traffic, err := netstat.Watch(netstat.NewAccount(cfg.Tag))
	if err != nil {
		report(Ready{Error: err.Error()})
		return 1, err
	}
	defer traffic.Close()
	mark("traffic")

	var proxies []io.Closer
	closeProxies := func() {
		for _, p := range proxies {
			p.Close()
		}
	}
	dial := func(raddr *net.UDPAddr) (net.Conn, error) { return alloc.DialFlow(cfg.AllocRoot, cfg.Stack, raddr) }
	var ports []int
	for _, b := range cfg.Publish {
		var cl io.Closer
		var port int
		if b.Dir != "" {
			// The container's router writes the client's address before
			// each connection's stream, which only the container library's
			// listeners take.
			cl, port, err = portproxy.ListenUnix(func(p int) string { return router.SocketPath(b.Dir, p) }, b.Lo, b.Hi, b.Upstream,
				func(r io.Reader) error { _, err := router.ReadClient(r); return err })
		} else {
			cl, port, err = portproxy.Listen(b.Proto, b.IP, b.Lo, b.Hi, b.Upstream, dial)
		}
		if err != nil {
			closeProxies()
			if errors.Is(err, syscall.EADDRINUSE) {
				report(Ready{Error: fmt.Sprintf("Bind for %s:%s failed", b.IP, portproxy.PortText(b.Lo, b.Hi)), PortInUse: true})
			} else {
				report(Ready{Error: err.Error()})
			}
			return 1, err
		}
		proxies = append(proxies, cl)
		ports = append(ports, port)
	}

	if cfg.Router != 0 {
		// The container's listeners on ports without host ports, and the
		// router that carries connections to them.
		os.RemoveAll(cfg.PortDir)
		if err := os.MkdirAll(cfg.PortDir, 0o700); err != nil {
			closeProxies()
			report(Ready{Error: err.Error()})
			return 1, err
		}
		defer os.RemoveAll(cfg.PortDir)
		flow := func() (*net.UDPConn, error) {
			c, err := alloc.ListenFlow(cfg.AllocRoot, cfg.Stack)
			if err == nil {
				// A UDP port's flow port carries the container's traffic.
				traffic.AddEndpoint(os.Getpid(), uint16(c.LocalAddr().(*net.UDPAddr).Port))
			}
			return c, err
		}
		// A relay to another stack's member sends from that stack's block,
		// carrying the container's traffic.
		dialFlow := func(stack string, raddr *net.UDPAddr) (net.Conn, error) {
			c, err := alloc.DialFlow(cfg.AllocRoot, stack, raddr)
			if err != nil {
				return nil, err
			}
			traffic.AddEndpoint(os.Getpid(), uint16(c.LocalAddr().(*net.UDPAddr).Port))
			return c, nil
		}
		r, err := router.Listen(net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Router)), cfg.PortDir, flow, router.Network{File: cfg.NetworkFile, Address: cfg.Address, DialFlow: dialFlow})
		if err != nil {
			closeProxies()
			report(Ready{Error: err.Error()})
			return 1, err
		}
		proxies = append(proxies, r)
		// The router's connections carry the container's traffic.
		traffic.AddEndpoint(os.Getpid(), uint16(cfg.Router))
	}

	mark("ports")
	spec := cfg.Spec
	stdout := &logfile.LineWriter{Stream: "stdout", To: logs}
	stderr := &logfile.LineWriter{Stream: "stderr", To: logs}
	var slave *os.File
	copied := make(chan struct{})
	if cfg.TTY {
		var master *os.File
		if master, slave, err = pty.Open(); err != nil {
			closeProxies()
			report(Ready{Error: err.Error()})
			return 1, err
		}
		if cfg.Console[0] > 0 && cfg.Console[1] > 0 {
			pty.Setsize(master, &pty.Winsize{Rows: uint16(cfg.Console[0]), Cols: uint16(cfg.Console[1])})
		}
		spec.TTY = slave
		s.master, s.stdin = master, master
	} else {
		close(copied)
		spec.Stdout, spec.Stderr = stdout, stderr
		if cfg.OpenStdin {
			pr, pw := io.Pipe()
			spec.Stdin = pr
			s.stdin = pw
		}
	}
	if cfg.Anchor != "" {
		os.Remove(cfg.Anchor)
		al, err := net.Listen("unix", cfg.Anchor)
		if err != nil {
			closeProxies()
			report(Ready{Error: err.Error()})
			return 1, err
		}
		ul := al.(*net.UnixListener)
		// The anchor, not the shim, accepts on it.
		ul.SetUnlinkOnClose(false)
		f, err := ul.File()
		ul.Close()
		if err != nil {
			closeProxies()
			report(Ready{Error: err.Error()})
			return 1, err
		}
		spec.AnchorListener = f
		defer os.Remove(cfg.Anchor)
	}
	mark("io")
	proc, err := runtime.Start(spec)
	mark("process")
	if spec.AnchorListener != nil {
		spec.AnchorListener.Close()
	}
	if slave != nil {
		slave.Close()
	}
	if err != nil {
		closeProxies()
		if s.master != nil {
			s.master.Close()
		}
		report(Ready{Error: err.Error()})
		return 1, err
	}
	s.proc = proc
	if s.master != nil {
		go func() {
			io.Copy(stdout, s.master)
			close(copied)
		}()
	}
	s.enf = newEnforcer(cfg.Tag, proc.Pid(), cfg.Resources, traffic)
	if pidsListener != nil {
		go s.enf.serveCount(pidsListener)
	}
	report(Ready{Pid: proc.Pid(), Ports: ports, Steps: strings.Join(steps, ", ")})
	go s.accept(l)

	<-proc.Done()
	// The container ends with its main process: whatever it left running
	// goes with it, in its process group or, having left the group, among
	// the members the enforcer followed while the main process answered
	// for them, or found by the tag its environment carries.
	left := s.enf.remaining()
	s.enf.close()
	proc.KillAll()
	if err := procs.KillEntries(left); err != nil {
		log.Printf("container %s: %v", cfg.ID, err)
	}
	if err := procs.KillTagged(cfg.Tag); err != nil {
		log.Printf("container %s: %v", cfg.ID, err)
	}
	// The namespace's other containers end with its pid 1, as a PID
	// namespace's processes do with its init.
	if tags, err := pidns.Tags(cfg.PidNamespace); cfg.PidNamespace != "" && err == nil && len(tags) > 0 && tags[0] == cfg.Tag {
		for _, t := range tags[1:] {
			if err := procs.KillTagged(t); err != nil {
				log.Printf("container %s: %v", cfg.ID, err)
			}
		}
	}
	proc.DrainIO(outputDrain)
	select {
	case <-copied:
	case <-time.After(outputDrain):
	}
	stdout.Flush()
	stderr.Flush()
	closeProxies()
	s.mu.Lock()
	if s.stdin != nil {
		s.stdin.Close()
	}
	s.mu.Unlock()
	exit := Exit{ExitCode: proc.ExitCode(), OOMKilled: s.enf.oomKilled(), FinishedAt: time.Now().UTC()}
	if err := writeFile(cfg.ExitPath, exit); err != nil {
		log.Printf("container %s: record the exit: %v", cfg.ID, err)
	}
	// Close the socket before answering the waiters: once a waiter learns
	// the exit, no new client reaches this shim.
	l.Close()
	os.Remove(cfg.Socket)
	s.mu.Lock()
	s.exit = exit
	close(s.exited)
	s.mu.Unlock()
	// Waiters get the exit status; streams end.
	s.mu.Lock()
	for c := range s.conns {
		c.SetReadDeadline(time.Now())
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.handled.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(outputDrain):
	}
	return 0, nil
}

func writeFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *server) notifyLogs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.logSubs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *server) accept(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.handled.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.handled.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				c.Close()
			}()
			s.handle(c)
		}()
	}
}

func respond(c net.Conn, r Response) error {
	b, _ := json.Marshal(r)
	_, err := c.Write(append(b, '\n'))
	return err
}

func fail(c net.Conn, err error) {
	if err != nil {
		respond(c, Response{Error: err.Error()})
		return
	}
	respond(c, Response{})
}

func (s *server) handle(c net.Conn) {
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		fail(c, err)
		return
	}
	switch req.Op {
	case "wait":
		<-s.exited
		s.mu.Lock()
		e := s.exit
		s.mu.Unlock()
		respond(c, Response{Exit: &e})
	case "state":
		st, err := s.enf.state()
		if err != nil {
			fail(c, err)
			return
		}
		respond(c, Response{State: &st})
	case "signal":
		fail(c, s.proc.Signal(syscall.Signal(req.Signal)))
	case "resize":
		if s.master == nil {
			fail(c, errors.New("the container has no TTY"))
			return
		}
		fail(c, pty.Setsize(s.master, &pty.Winsize{Rows: req.Height, Cols: req.Width}))
	case "pause":
		fail(c, s.enf.pause(true))
	case "unpause":
		fail(c, s.enf.pause(false))
	case "update":
		if req.Resources != nil {
			s.enf.update(*req.Resources)
		}
		if req.Nice != nil {
			fail(c, s.enf.renice(*req.Nice))
			return
		}
		fail(c, nil)
	case "track":
		s.enf.track()
		fail(c, nil)
	case "stdin":
		s.mu.Lock()
		w := s.stdin
		s.mu.Unlock()
		if w == nil {
			fail(c, errors.New("the container's input is closed"))
			return
		}
		fail(c, nil)
		io.Copy(w, r)
		if req.Close && s.master == nil {
			s.mu.Lock()
			if s.stdin != nil {
				s.stdin.Close()
				s.stdin = nil
			}
			s.mu.Unlock()
		}
		c.(*net.UnixConn).CloseWrite()
	case "logs":
		ch := make(chan struct{}, 1)
		s.mu.Lock()
		s.logSubs[ch] = struct{}{}
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.logSubs, ch)
			s.mu.Unlock()
		}()
		if respond(c, Response{}) != nil {
			return
		}
		gone := make(chan struct{})
		go func() {
			io.Copy(io.Discard, r)
			close(gone)
		}()
		for {
			select {
			case <-ch:
				if _, err := c.Write([]byte{1}); err != nil {
					return
				}
			case <-gone:
				return
			case <-s.exited:
				return
			}
		}
	default:
		fail(c, fmt.Errorf("unknown request %q", req.Op))
	}
}
