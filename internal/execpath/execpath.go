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
	mounts := mountsOf(env)
	if strings.Contains(argv0, "/") {
		p, ok := hostPath(root, "/"+argv0, mounts)
		if !ok || !isExecutable(p) {
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
		// A PATH the engine mapped into the root names container paths
		// under it.
		if rest, ok := strings.CutPrefix(dir, root+"/"); ok {
			dir = "/" + rest
		}
		if p, ok := hostPath(root, path.Join(dir, argv0), mounts); ok && isExecutable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", argv0)
}

// HostPath is the host path of container path p, as the container's
// processes reach it: through its links and onto its mounts, which env's
// FWS_MOUNTS lists. Components that do not exist are kept as given, so
// the path may be made. Links that loop leave p in the root as given.
func HostPath(root, p string, env []string) string {
	if h, ok := hostPath(root, path.Clean("/"+p), mountsOf(env)); ok {
		return h
	}
	return filepath.Join(root, path.Clean("/"+p))
}

// mount is a mount's destination in the container and its source on the
// host.
type mount struct{ dest, src string }

// mountsVar names the variable that lists a container's mounts, a
// destination, a tab and its source a line each, as the container library
// reads it (package pathmap).
const mountsVar = "FWS_MOUNTS"

func mountsOf(env []string) []mount {
	var out []mount
	for _, kv := range env {
		v, ok := strings.CutPrefix(kv, mountsVar+"=")
		if !ok {
			continue
		}
		out = out[:0]
		for _, line := range strings.Split(v, "\n") {
			d, src, ok := strings.Cut(line, "\t")
			if ok && strings.HasPrefix(d, "/") && strings.HasPrefix(src, "/") {
				out = append(out, mount{path.Clean(d), src})
			}
		}
	}
	return out
}

// maxLinks bounds symbolic link resolution, as the kernel's ELOOP does.
const maxLinks = 40

// hostPath is the host path container path p names: its symbolic links
// resolved as the container's kernel would resolve them, an absolute target
// from the container's root and .. never above it, and a path under a
// mount's destination in the mount's source, as the container library maps
// it. A link the engine places for a mount is never read: the mount is
// found by its destination first. An absolute target the root does not
// hold, and the host does, is the host's, as for the link the engine places
// for docker-init. It reports false for a path whose links loop.
func hostPath(root, p string, mounts []mount) (string, bool) {
	rest := strings.Split(p, "/")
	var done []string
	for hops := 0; len(rest) > 0; {
		part := rest[0]
		rest = rest[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(done) > 0 {
				done = done[:len(done)-1]
			}
			continue
		}
		done = append(done, part)
		host := hostOf(root, "/"+strings.Join(done, "/"), mounts)
		fi, err := os.Lstat(host)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(host)
		if err != nil {
			continue
		}
		if hops++; hops > maxLinks {
			return "", false
		}
		done = done[:len(done)-1]
		if strings.HasPrefix(target, "/") {
			// A target the root does not hold is the host's, as the
			// container library takes a path the root lacks: the links the
			// engine places, for docker-init, name host paths.
			if _, err := os.Lstat(hostOf(root, path.Clean(target), mounts)); err != nil {
				if _, err := os.Lstat(target); err == nil {
					return filepath.Join(append([]string{target}, rest...)...), true
				}
			}
			done = nil
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	return hostOf(root, "/"+strings.Join(done, "/"), mounts), true
}

// hostOf is the host path of container path p, which holds no link: under
// the mount whose destination holds it, the longest, or else the root.
func hostOf(root, p string, mounts []mount) string {
	best := -1
	for i, m := range mounts {
		if (p == m.dest || strings.HasPrefix(p, m.dest+"/")) && (best < 0 || len(m.dest) > len(mounts[best].dest)) {
			best = i
		}
	}
	if best >= 0 {
		return filepath.Join(mounts[best].src, strings.TrimPrefix(p, mounts[best].dest))
	}
	return filepath.Join(root, p)
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
