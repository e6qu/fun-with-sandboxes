// Command entry runs Redis inside a fwsd container on the port and data
// directory the engine assigned.
package main

import (
	"bufio"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	port, err := sandboxenv.Port("6379")
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "health" {
		if err := ping("127.0.0.1:" + port); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	dir := sandboxenv.Path("/data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "redis-server" {
		args = args[1:]
	}
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		log.Fatal(err)
	}
	server := []string{"redis-server", "--port", port, "--bind", "127.0.0.1", "--dir", dir}
	log.Fatal(syscall.Exec(bin, append(server, args...), os.Environ()))
}

func ping(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("PING\r\n")); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) != "+PONG" {
		return &net.AddrError{Err: "unexpected reply " + strings.TrimSpace(line), Addr: addr}
	}
	return nil
}
