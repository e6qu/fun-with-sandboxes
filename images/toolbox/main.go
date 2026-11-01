// Command toolbox is a small stack application and a probe. As an app it
// serves an HTTP counter backed by its stack's Redis; as a probe it attempts
// one operation and exits 0 if the system allowed it, 1 if it refused.
package main

import (
	"bufio"
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
