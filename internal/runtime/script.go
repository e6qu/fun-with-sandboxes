package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"github.com/e6qu/fun-with-sandboxes/internal/execpath"
)

// scriptCommand is how a container runs path with argv when path is a
// script: the kernel resolves the interpreter its #! line names on the
// host, so one the container's root holds runs from the root, as in a
// chroot, with its #! argument, the script and the script's arguments.
// A #! line of /usr/bin/env, which the root does not hold, runs the program
// its argument names from the container's PATH, as env would, since Apple's
// env does not run with the container library loaded. Any other program
// runs as it is.
func scriptCommand(root, path string, argv, env []string) (string, []string) {
	f, err := os.Open(path)
	if err != nil {
		return path, argv
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	f.Close()
	if n < 3 || !bytes.HasPrefix(head, []byte("#!")) {
		return path, argv
	}
	line, _, _ := bytes.Cut(head[2:n], []byte("\n"))
	fields := strings.Fields(strings.TrimRight(string(line), "\r"))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return path, argv
	}
	var interp string
	var out []string
	if fi, err := os.Stat(filepath.Join(root, fields[0])); err == nil && !fi.IsDir() {
		interp = filepath.Join(root, fields[0])
		out = []string{fields[0]}
		if len(fields) > 1 {
			// As the kernel does, the rest of the line is one argument.
			out = append(out, strings.Join(fields[1:], " "))
		}
	} else if fields[0] == "/usr/bin/env" && len(fields) > 1 {
		words := fields[1:]
		if words[0] == "-S" {
			words = words[1:]
		} else {
			words = []string{strings.Join(words, " ")}
		}
		if len(words) == 0 || strings.HasPrefix(words[0], "-") || strings.Contains(words[0], "=") {
			return path, argv
		}
		if interp, err = execpath.Resolve(root, words[0], env); err != nil {
			return path, argv
		}
		out = append(out, words...)
	} else {
		return path, argv
	}
	out = append(out, path)
	if len(argv) > 1 {
		out = append(out, argv[1:]...)
	}
	return interp, out
}
