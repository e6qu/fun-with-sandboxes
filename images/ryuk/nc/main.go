// Command nc checks that a container port accepts TCP connections, as
// `nc -z host port` does, for the in-container port check Testcontainers
// runs: a container port listens on the loopback port the engine assigned
// it, which SANDBOX_PORT_<port> names.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

func main() {
	verbose := flag.Bool("v", false, "report the outcome")
	zero := flag.Bool("z", false, "check that the port accepts connections, sending nothing")
	wait := flag.Int("w", 5, "connect timeout in seconds")
	flag.Parse()
	if !*zero || flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: nc -z [-v] [-w seconds] host port")
		os.Exit(2)
	}
	host, port := flag.Arg(0), flag.Arg(1)
	if _, err := strconv.Atoi(port); err != nil {
		fmt.Fprintf(os.Stderr, "nc: port %q is not a number\n", port)
		os.Exit(2)
	}
	if host != "localhost" && host != "127.0.0.1" {
		fmt.Fprintf(os.Stderr, "nc: %s is not this container: only its own ports can be checked\n", host)
		os.Exit(1)
	}
	assigned, err := sandboxenv.Port(port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nc:", err)
		os.Exit(1)
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", assigned), time.Duration(*wait)*time.Second)
	if err != nil {
		if *verbose {
			fmt.Fprintf(os.Stderr, "nc: connect to %s port %s (tcp) failed: %v\n", host, port, err)
		}
		os.Exit(1)
	}
	c.Close()
	if *verbose {
		fmt.Fprintf(os.Stderr, "Connection to %s port %s [tcp/*] succeeded!\n", host, port)
	}
}
