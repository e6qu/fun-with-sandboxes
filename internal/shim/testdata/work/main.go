// Command work is the shim tests' workload: each argument list names one
// thing to do.
package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	switch args[0] {
	case "echo":
		// Print the rest of the arguments, one per line, then exit with
		// status 3.
		for _, a := range args[1:] {
			fmt.Println(a)
		}
		os.Exit(3)
	case "cat":
		_, err := io.Copy(os.Stdout, os.Stdin)
		return err
	case "tty":
		_, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		fmt.Printf("tty=%v\n", err == nil)
		return nil
	case "alloc":
		// Allocate and touch the given number of MiB, a MiB at a time,
		// then hold it.
		n, _ := strconv.Atoi(args[1])
		var keep [][]byte
		for i := 0; i < n; i++ {
			b := make([]byte, 1<<20)
			for j := range b {
				b[j] = byte(j)
			}
			keep = append(keep, b)
			time.Sleep(time.Millisecond)
		}
		fmt.Println("allocated", len(keep))
		hold()
	case "forkmany":
		// Start the given number of children, reaping those that end.
		n, _ := strconv.Atoi(args[1])
		for i := 0; i < n; i++ {
			cmd := exec.Command(os.Args[0], "hold")
			if err := cmd.Start(); err != nil {
				return err
			}
			go cmd.Wait()
		}
		fmt.Println("started", n)
		hold()
	case "burn":
		// Spin for the given wall time and report the CPU time it got.
		d, _ := time.ParseDuration(args[1])
		end := time.Now().Add(d)
		for time.Now().Before(end) {
		}
		var ru syscall.Rusage
		syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
		cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
		fmt.Println("cpu", cpu.Milliseconds())
		hold()
	case "serve":
		// Answer each connection on the loopback port with a greeting.
		l, err := net.Listen("tcp", "127.0.0.1:"+args[1])
		if err != nil {
			return err
		}
		fmt.Println("listening")
		for {
			c, err := l.Accept()
			if err != nil {
				return err
			}
			fmt.Fprintln(c, "hello from the container")
			c.Close()
		}
	case "lines":
		// Print a line for each line of input, then hold.
		s := bufio.NewScanner(os.Stdin)
		for s.Scan() {
			fmt.Println("got", s.Text())
		}
		hold()
	case "hold":
		hold()
	}
	return fmt.Errorf("unknown op %q", args[0])
}

func hold() {
	for {
		time.Sleep(time.Hour)
	}
}
