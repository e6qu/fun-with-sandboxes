// Command busybox answers scripts that call BusyBox by name, as in
// `busybox wget URL` or `busybox --list`, in fws/busybox. BusyBox builds
// for Linux alone, so the image's commands are separate programs in /bin;
// busybox runs the one its first argument names and lists them.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// binDir is where the image's commands are.
var binDir = "/bin"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: busybox [--list | COMMAND [ARG]...]")
		fmt.Fprintln(os.Stderr, "Runs COMMAND from "+binDir+"; fws/busybox's commands are separate programs.")
		if len(args) == 0 {
			return 1
		}
		return 0
	}
	if args[0] == "--list" {
		names, err := commands()
		if err != nil {
			fmt.Fprintln(os.Stderr, "busybox:", err)
			return 1
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return 0
	}
	name := args[0]
	if name == "busybox" || filepath.Base(name) != name {
		fmt.Fprintf(os.Stderr, "busybox: %s: applet not found\n", name)
		return 127
	}
	path := filepath.Join(binDir, name)
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "busybox: %s: applet not found\n", name)
		return 127
	}
	err := syscall.Exec(path, args, os.Environ())
	fmt.Fprintf(os.Stderr, "busybox: %s: %v\n", name, err)
	return 126
}

// commands are the names in binDir, busybox's own aside.
func commands() ([]string, error) {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Name() != "busybox" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
