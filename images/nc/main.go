// Command nc checks that a container port accepts TCP connections, as
// `nc -z host port` does: the check Testcontainers runs inside a
// container where a Linux image would read /proc/net/tcp, which macOS
// lacks. It takes netcat's short options combined, as in -vz, and reaches
// the port through the container library, as any client in the container
// does.
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

func main() {
	verbose, zero, wait, args, err := parse(os.Args[1:])
	if err != nil || !zero || len(args) != 2 {
		if err != nil {
			fmt.Fprintln(os.Stderr, "nc:", err)
		}
		fmt.Fprintln(os.Stderr, "usage: nc -z [-v] [-w timeout] host port")
		os.Exit(2)
	}
	host, port := args[0], args[1]
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		fmt.Fprintf(os.Stderr, "nc: port number invalid: %s\n", port)
		os.Exit(1)
	}
	timeout := 10 * time.Second
	if wait > 0 {
		timeout = time.Duration(wait) * time.Second
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "nc: connect to %s port %s (tcp) failed: %v\n", host, port, err)
		}
		os.Exit(1)
	}
	c.Close()
	if verbose {
		fmt.Fprintf(os.Stderr, "Connection to %s port %s [tcp/*] succeeded!\n", host, port)
	}
}

// parse reads netcat's options -v, -z and -w seconds, combined or apart,
// before the operands.
func parse(argv []string) (verbose, zero bool, wait int, args []string, err error) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if len(a) < 2 || a[0] != '-' || a == "--" {
			if a == "--" {
				i++
			}
			return verbose, zero, wait, argv[i:], nil
		}
		for j := 1; j < len(a); j++ {
			switch a[j] {
			case 'v':
				verbose = true
			case 'z':
				zero = true
			case 'w':
				v := a[j+1:]
				if v == "" {
					if i+1 >= len(argv) {
						return false, false, 0, nil, fmt.Errorf("option -w needs a timeout")
					}
					i++
					v = argv[i]
				}
				if wait, err = strconv.Atoi(v); err != nil || wait < 0 {
					return false, false, 0, nil, fmt.Errorf("timeout invalid: %s", v)
				}
				j = len(a)
			default:
				return false, false, 0, nil, fmt.Errorf("invalid option -- %c", a[j])
			}
		}
	}
	return verbose, zero, wait, nil, nil
}
