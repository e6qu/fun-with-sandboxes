// Command tar is fws/busybox's and fws/base's tar: libarchive's bsdtar, as
// macOS's is, which reads zip as well as tar. A zip keeps its files' modes
// in its central directory, at its end, which bsdtar reading a zip from a
// pipe never reaches, so what it unpacks loses its execute bits. A zip on
// stdin is kept in a file first, which bsdtar reads whole, modes and all:
// the Dev Containers extension unpacks VS Code's macOS server, a zip, with
// tar -xz from a pipe. Anything else goes to bsdtar as it came.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// bsdtar is libarchive's tar.
var bsdtar = "/bin/bsdtar"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin))
}

func run(args []string, stdin io.Reader) int {
	if !extractsStdin(args) {
		err := syscall.Exec(bsdtar, append([]string{"tar"}, args...), os.Environ())
		fmt.Fprintln(os.Stderr, "tar:", err)
		return 126
	}
	in := bufio.NewReader(stdin)
	magic, _ := in.Peek(4)
	if !bytes.Equal(magic, []byte("PK\x03\x04")) {
		return bsd(args, in)
	}
	f, err := os.CreateTemp("", "tar-zip-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tar:", err)
		return 1
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, in)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tar:", err)
		return 1
	}
	return bsd(withFile(args, f.Name()), nil)
}

// bsd runs bsdtar with args and stdin and returns its exit status.
func bsd(args []string, stdin io.Reader) int {
	cmd := exec.Command(bsdtar, args...)
	cmd.Args[0] = "tar"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return max(exit.ExitCode(), 1)
	default:
		fmt.Fprintln(os.Stderr, "tar:", err)
		return 126
	}
}

// extractsStdin reports whether args extract an archive read from stdin:
// -x, --extract or --get, with no file named, or the file "-".
func extractsStdin(args []string) bool {
	extract := false
	file, named := "", false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			i = len(args)
		case a == "--extract" || a == "--get":
			extract = true
		case strings.HasPrefix(a, "--file="):
			file, named = strings.TrimPrefix(a, "--file="), true
		case a == "--file":
			if i+1 < len(args) {
				file, named = args[i+1], true
				i++
			}
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") || i == 0:
			// A bundle of short options, the first argument's dash being
			// optional; an f in it takes the next argument, or the rest
			// of the bundle, as the file.
			opts := strings.TrimPrefix(a, "-")
			for j, c := range opts {
				if c == 'x' {
					extract = true
				}
				if c == 'f' {
					named = true
					if rest := opts[j+1:]; rest != "" && strings.HasPrefix(a, "-") {
						file = rest
					} else if i+1 < len(args) {
						file = args[i+1]
						i++
					}
					break
				}
			}
		}
	}
	return extract && (!named || file == "-")
}

// withFile names file as the archive in args, in place of stdin: where
// args name "-", or, naming none, as --file.
func withFile(args []string, file string) []string {
	out := make([]string, 0, len(args)+1)
	replaced := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		short := !strings.HasPrefix(a, "--") && (strings.HasPrefix(a, "-") || i == 0)
		switch {
		case replaced:
		case a == "--file=-":
			a, replaced = "--file="+file, true
		case (a == "--file" || short && strings.HasSuffix(a, "f")) && i+1 < len(args) && args[i+1] == "-":
			out = append(out, a)
			a, replaced = file, true
			i++
		case short && strings.HasPrefix(a, "-") && strings.HasSuffix(a, "f-"):
			out = append(out, strings.TrimSuffix(a, "-"))
			a, replaced = file, true
		}
		out = append(out, a)
	}
	if !replaced {
		out = append(out, "--file="+file)
	}
	return out
}
