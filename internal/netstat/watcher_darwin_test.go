package netstat

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

const childVar = "NETSTAT_TEST_CHILD"

// The test binary, run with childVar set, plays a container's process:
// "client <addr> <n>" sends n bytes to addr and reads n back; "self <n>"
// does the same over a connection to its own listener. Each prints "done"
// and holds the connection open until its input ends.
func TestMain(m *testing.M) {
	if args := os.Getenv(childVar); args != "" {
		if err := child(args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func child(args string) error {
	var mode, addr string
	var n int
	if _, err := fmt.Sscan(args, &mode); err != nil {
		return err
	}
	switch mode {
	case "client":
		fmt.Sscan(args, &mode, &addr, &n)
	case "self":
		fmt.Sscan(args, &mode, &n)
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer l.Close()
		go echo(l)
		addr = l.Addr().String()
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	go c.Write(bytes.Repeat([]byte("x"), n))
	if _, err := io.CopyN(io.Discard, c, int64(n)); err != nil {
		return err
	}
	fmt.Println("done")
	io.Copy(io.Discard, os.Stdin)
	return c.Close()
}

func echo(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			io.Copy(c, c)
			c.Close()
		}()
	}
}

type running struct {
	cmd *exec.Cmd
	in  io.WriteCloser
}

// start runs the test binary as a process carrying tag, and waits for it to
// finish its transfer.
func start(t *testing.T, tag, args string) *running {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childVar+"="+args, tag)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &running{cmd, in}
	t.Cleanup(r.stop)
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "done\n" {
		t.Fatalf("the child said %q: %v", line, err)
	}
	return r
}

func (r *running) stop() {
	r.in.Close()
	r.cmd.Wait()
}

func watch(t *testing.T, tag string) *Watcher {
	t.Helper()
	w, err := Watch(NewAccount(tag))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func total(t *testing.T, w *Watcher) Counts {
	t.Helper()
	c, err := w.Total()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The kernel's counts of a container process's connection to a server
// outside it reach the account, while it is open and after the process
// ended.
func TestWatcherCountsAContainersConnection(t *testing.T) {
	const n = 256 << 10
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go echo(l)
	tag := "SANDBOX_ROOT=/netstat-test-" + strconv.Itoa(os.Getpid()) + "-a"
	w := watch(t, tag)
	r := start(t, tag, "client "+l.Addr().String()+" "+strconv.Itoa(n))
	open := total(t, w)
	if open.RxBytes < n || open.TxBytes < n || open.RxPackets == 0 || open.TxPackets == 0 {
		t.Fatalf("an open connection counted %+v, want at least %d bytes each way", open, n)
	}
	// The server, outside the container, is none of its traffic.
	if open.RxBytes > 2*n || open.TxBytes > 2*n {
		t.Fatalf("the connection counted %+v, more than the container's own end", open)
	}
	r.stop()
	closed := total(t, w)
	if closed.RxBytes < open.RxBytes || closed.TxBytes < open.TxBytes {
		t.Fatalf("after the process ended the connection counted %+v, less than the %+v it had", closed, open)
	}
}

// A connection between two of the container's own sockets crosses no
// boundary, as one over a Docker container's loopback crosses no eth0.
func TestWatcherLeavesOutInternalConnections(t *testing.T) {
	const n = 128 << 10
	tag := "SANDBOX_ROOT=/netstat-test-" + strconv.Itoa(os.Getpid()) + "-b"
	w := watch(t, tag)
	r := start(t, tag, "self "+strconv.Itoa(n))
	if got := total(t, w); got != (Counts{}) {
		t.Fatalf("an internal connection counted %+v, want nothing", got)
	}
	r.stop()
	if got := total(t, w); got != (Counts{}) {
		t.Fatalf("an internal connection counted %+v after it closed, want nothing", got)
	}
}
