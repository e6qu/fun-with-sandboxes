// Command entry runs Valkey inside a fwsd container as the official
// image's entrypoint does: options or a .conf file as the first argument
// run valkey-server with them, and valkey-server runs as the valkey user
// under umask 0077. The server listens on the container's port 6379,
// which the container library puts on its host port.
package main

import (
	"bufio"
	"fmt"
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
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "health" {
		if err := ping("127.0.0.1:6379"); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || strings.HasSuffix(args[0], ".conf") {
		args = append([]string{"valkey-server"}, args...)
	}
	if args[0] == "valkey-server" {
		wd, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		if err := sandboxenv.Become("valkey", wd); err != nil {
			log.Fatal(err)
		}
	}
	if old := syscall.Umask(0o077); old != 0o022 {
		syscall.Umask(old)
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(bin, args, os.Environ()))
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
		return fmt.Errorf("%s answered %q", addr, strings.TrimSpace(line))
	}
	return nil
}
