// Command entry runs Redis inside a fwsd container as the official
// image's entrypoint does: options or a .conf file as the first argument
// run redis-server with them, and redis-server runs as the redis user
// under umask 0077, with protected mode off unless a config file it is
// given sets it. The server listens on the container's port 6379,
// which the container library puts on its host port.
package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
		args = append([]string{"redis-server"}, args...)
	}
	if args[0] == "redis-server" {
		args = unprotected(args)
		wd, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		if err := sandboxenv.Become("redis", wd); err != nil {
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

// unprotected turns protected mode off, as the official image does, where
// no config file, the argument after redis-server, sets protected-mode: a
// server without a password otherwise refuses clients other than the
// loopback's.
func unprotected(args []string) []string {
	rest := args[1:]
	var conf []string
	if len(rest) > 0 {
		// A relative path is the working directory's, which is the
		// process's own on the host too.
		p := rest[0]
		if filepath.IsAbs(p) {
			p = sandboxenv.Path(p)
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err == nil && protectedModeSet(string(b)) {
				return args
			}
			conf, rest = rest[:1], rest[1:]
		}
	}
	out := append([]string{args[0]}, conf...)
	out = append(out, "--protected-mode", "no")
	return append(out, rest...)
}

// protectedModeSet reports whether a config file sets protected-mode on a
// line of its own, as the official image's grep '^protected-mode' finds it.
func protectedModeSet(conf string) bool {
	for _, line := range strings.Split(conf, "\n") {
		if strings.HasPrefix(line, "protected-mode") {
			return true
		}
	}
	return false
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
