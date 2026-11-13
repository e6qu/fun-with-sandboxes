// Package execpath finds a container's executables as execve and a shell's
// PATH search would inside its root. It depends on nothing of the engine's,
// so the images' entry programs that use it are not rebuilt when the
// runtime changes.
package execpath

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// FromWorkDir makes a relative argv[0] with a slash, such as ./run.sh,
// absolute in the container, as execve resolves it from the working
// directory.
func FromWorkDir(workDir, argv0 string) string {
	if !strings.Contains(argv0, "/") || strings.HasPrefix(argv0, "/") {
		return argv0
	}
	return path.Join("/", workDir, argv0)
}

// Resolve maps argv[0] to the executable's host path inside root, searching the
// container's PATH when argv[0] has no slash.
func Resolve(root, argv0 string, env []string) (string, error) {
	if argv0 == "" {
		return "", errors.New("no command specified")
	}
	if strings.Contains(argv0, "/") {
		p := filepath.Join(root, path.Clean("/"+argv0))
		if !isExecutable(p) {
			return "", fmt.Errorf("exec: %q: no such executable in the container", argv0)
		}
		return p, nil
	}
	pathVar := "/usr/local/bin:/usr/bin:/bin"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVar = v
		}
	}
	for _, dir := range filepath.SplitList(pathVar) {
		if !strings.HasPrefix(dir, "/") {
			continue
		}
		base := filepath.Join(root, path.Clean(dir))
		if strings.HasPrefix(dir, root+"/") {
			base = path.Clean(dir)
		}
		p := filepath.Join(base, argv0)
		if isExecutable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", argv0)
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
