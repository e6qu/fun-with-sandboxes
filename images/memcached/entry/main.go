// Command entry runs Memcached inside a fwsd container as the official
// image's entrypoint does: options as the first argument run memcached
// with them. The server listens on the container's port 11211, which the
// container library puts on its host port.
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
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "health" {
		if err := version("127.0.0.1:11211"); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		args = append([]string{"memcached"}, args...)
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(bin, args, os.Environ()))
}

// version asks the server its version, which it answers once it serves.
func version(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("version\r\n")); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "VERSION ") {
		return fmt.Errorf("%s answered %q", addr, strings.TrimSpace(line))
	}
	return nil
}
