// Command toolbox is a small stack application and a probe. As an app it
// serves an HTTP counter backed by its stack's Redis; as a probe it attempts
// one operation and exits 0 if the system allowed it, 1 if it refused.
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/net/proxy"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: toolbox serve|health|echo|env|cat|put|fetch|dial|write|read|list|bind|connect|signal|exec [arg]")
		os.Exit(2)
	}
	arg := ""
	if len(os.Args) > 2 {
		arg = os.Args[2]
	}
	if err := run(os.Args[1], arg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(op, arg string) error {
	switch op {
	case "serve":
		return serve()
	case "health":
		port, err := sandboxenv.Port("8080")
		if err != nil {
			return err
		}
		c := http.Client{Timeout: 2 * time.Second}
		res, err := c.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			return err
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("healthz: %s", res.Status)
		}
		return nil
	case "echo":
		fmt.Println(strings.Join(os.Args[2:], " "))
		return nil
	case "env":
		env := os.Environ()
		sort.Strings(env)
		for _, kv := range env {
			if strings.HasPrefix(kv, "SANDBOX_") {
				fmt.Println(kv)
			}
		}
		return nil
	case "cat":
		b, err := os.ReadFile(sandboxenv.Path(arg))
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	case "put":
		if len(os.Args) < 4 {
			return fmt.Errorf("put needs a path and content")
		}
		p := sandboxenv.Path(arg)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(os.Args[3]+"\n"), 0o644)
	case "fetch":
		c := http.Client{Timeout: 30 * time.Second}
		res, err := c.Get(arg)
		if err != nil {
			return err
		}
		res.Body.Close()
		if res.StatusCode >= 400 {
			return fmt.Errorf("%s: %s", arg, res.Status)
		}
		fmt.Println(res.Status)
		return nil
	case "tcp":
		// Open a TCP connection to arg through ALL_PROXY, the stack's SOCKS5
		// egress, and complete a TLS handshake over it when arg is a :443
		// address, proving the bytes reached the server.
		d := proxy.FromEnvironment()
		c, err := d.Dial("tcp", arg)
		if err != nil {
			return err
		}
		defer c.Close()
		if host, port, _ := net.SplitHostPort(arg); port == "443" {
			tc := tls.Client(c, &tls.Config{ServerName: host})
			tc.SetDeadline(time.Now().Add(30 * time.Second))
			if err := tc.Handshake(); err != nil {
				return err
			}
			fmt.Println("TLS with", tc.ConnectionState().PeerCertificates[0].Subject.CommonName)
		}
		return nil
	case "dial":
		c, err := net.DialTimeout("tcp", arg, 5*time.Second)
		if err != nil {
			return err
		}
		return c.Close()
	case "write":
		return os.WriteFile(arg, []byte("x"), 0o600)
	case "read":
		_, err := os.ReadFile(arg)
		return err
	case "list":
		_, err := os.ReadDir(arg)
		return err
	case "bind":
		l, err := net.Listen("tcp", "127.0.0.1:"+arg)
		if err != nil {
			return err
		}
		return l.Close()
	case "connect":
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+arg, 2*time.Second)
		if err != nil {
			return err
		}
		return c.Close()
	case "udpecho":
		// Answer each datagram on container port arg/udp with "echo:" and
		// the datagram.
		port, err := sandboxenv.Port(arg)
		if err != nil {
			return err
		}
		pc, err := net.ListenPacket("udp", "127.0.0.1:"+port)
		if err != nil {
			return err
		}
		fmt.Println("listening on udp", port)
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return err
			}
			if _, err := pc.WriteTo(append([]byte("echo:"), buf[:n]...), from); err != nil {
				return err
			}
		}
	case "detach":
		// Start a process in a session of its own, outside the container's
		// process group, print its pid, and stay running.
		self, err := os.Executable()
		if err != nil {
			return err
		}
		child, err := os.StartProcess(self, []string{self, "hold"}, &os.ProcAttr{
			Files: []*os.File{nil, nil, nil}, Sys: &syscall.SysProcAttr{Setsid: true},
		})
		if err != nil {
			return err
		}
		fmt.Println("detached", child.Pid)
		hold()
	case "hold":
		hold()
	case "alloc":
		// Allocate and touch arg MiB, a MiB at a time, then hold it.
		n, err := strconv.Atoi(arg)
		if err != nil {
			return err
		}
		var keep [][]byte
		for i := 0; i < n; i++ {
			b := make([]byte, 1<<20)
			for j := range b {
				b[j] = byte(j)
			}
			keep = append(keep, b)
			time.Sleep(time.Millisecond)
		}
		fmt.Println("allocated", len(keep), "MiB")
		hold()
	case "burn":
		// Spin for the wall time arg names and print the CPU time it got,
		// in milliseconds.
		d, err := time.ParseDuration(arg)
		if err != nil {
			return err
		}
		for end := time.Now().Add(d); time.Now().Before(end); {
		}
		var ru syscall.Rusage
		syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
		fmt.Println("cpu", (ru.Utime.Nano()+ru.Stime.Nano())/1e6)
		hold()
	case "forkmany":
		// Start arg children, reaping those that end, and stay running.
		n, err := strconv.Atoi(arg)
		if err != nil {
			return err
		}
		self, err := os.Executable()
		if err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			p, err := os.StartProcess(self, []string{self, "hold"}, &os.ProcAttr{Files: []*os.File{nil, nil, nil}})
			if err != nil {
				return err
			}
			go p.Wait()
		}
		fmt.Println("started", n)
		hold()
	case "openfiles":
		// Open /dev/null arg times, holding every descriptor.
		n, err := strconv.Atoi(arg)
		if err != nil {
			return err
		}
		var fs []*os.File
		for i := 0; i < n; i++ {
			f, err := os.Open("/dev/null")
			if err != nil {
				return fmt.Errorf("after %d files: %w", len(fs), err)
			}
			fs = append(fs, f)
		}
		fmt.Println("opened", len(fs))
		return nil
	case "shm":
		// Create and remove the POSIX shared memory object arg, named as
		// given, or under the container's IPC prefix when arg starts with
		// a "+".
		name := arg
		if rest, ok := strings.CutPrefix(arg, "+"); ok {
			name = sandboxenv.IPCName(rest)
		}
		return shm(name)
	case "sshagent":
		// List the identities of the SSH agent SSH_AUTH_SOCK names, as a
		// build step with an SSH mount reaches the client's agent.
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return fmt.Errorf("SSH_AUTH_SOCK is not set")
		}
		c, err := net.Dial("unix", sandboxenv.Path(sock))
		if err != nil {
			return err
		}
		defer c.Close()
		keys, err := agent.NewClient(c).List()
		if err != nil {
			return err
		}
		fmt.Println("identities", len(keys))
		for _, k := range keys {
			fmt.Println(k.Comment)
		}
		return nil
	case "signal":
		pid, err := strconv.Atoi(arg)
		if err != nil {
			return err
		}
		return syscall.Kill(pid, 0)
	case "exec":
		return syscall.Exec(arg, []string{arg}, nil)
	}
	return fmt.Errorf("unknown op %q", op)
}

func serve() error {
	port, err := sandboxenv.Port("8080")
	if err != nil {
		return err
	}
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	http.HandleFunc("/counter", func(w http.ResponseWriter, r *http.Request) {
		addr, err := sandboxenv.Peer("cache", "6379")
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		n, err := incr(addr, "hits")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"stack": os.Getenv("SANDBOX_STACK"), "hits": n})
	})
	fmt.Printf("toolbox listening on 127.0.0.1:%s\n", port)
	return http.ListenAndServe("127.0.0.1:"+port, nil)
}

// incr sends a Redis INCR in the RESP protocol and returns the new value.
func incr(addr, key string) (int64, error) {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(c, "*2\r\n$4\r\nINCR\r\n$%d\r\n%s\r\n", len(key), key)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return 0, err
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, ":") {
		return 0, fmt.Errorf("redis: %s", line)
	}
	return strconv.ParseInt(line[1:], 10, 64)
}

// hold runs until the process is killed. A bare select{} would not: with
// no other goroutine, Go's runtime ends the program as deadlocked.
func hold() {
	for {
		time.Sleep(time.Hour)
	}
}
