package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// scriptCommand is how a container runs path with argv when path is a
// script: the kernel resolves the interpreter its #! line names on the
// host, so one the container's root holds runs from the root, as in a
// chroot, with its #! argument, the script and the script's arguments.
// Any other program runs as it is.
func scriptCommand(root, path string, argv []string) (string, []string) {
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
	interp := filepath.Join(root, fields[0])
	if fi, err := os.Stat(interp); err != nil || fi.IsDir() {
		return path, argv
	}
	out := []string{fields[0]}
	if len(fields) > 1 {
		// As the kernel does, the rest of the line is one argument.
		out = append(out, strings.Join(fields[1:], " "))
	}
	out = append(out, path)
	if len(argv) > 1 {
		out = append(out, argv[1:]...)
	}
	return interp, out
}
