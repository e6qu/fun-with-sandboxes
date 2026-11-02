// Package sandboxenv reads the variables the engine gives every container
// process: its root directory and the host ports its stack assigned.
package sandboxenv

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/e6qu/fun-with-sandboxes/internal/identity"
)

// Root is the container's root directory on the host.
func Root() string { return os.Getenv("SANDBOX_ROOT") }

// Path maps a path inside the container to its place on the host; a path
// already under the root is returned unchanged.
func Path(p string) string {
	root := Root()
	if strings.HasPrefix(p, root+"/") || p == root {
		return p
	}
	return filepath.Join(root, filepath.Clean("/"+p))
}

// Port returns the host port assigned to one of the container's own ports.
func Port(containerPort string) (string, error) {
	v := os.Getenv("SANDBOX_PORT_" + containerPort)
	if v == "" {
		return "", fmt.Errorf("SANDBOX_PORT_%s is not set: is this running under fwsd with port %s exposed?", containerPort, containerPort)
	}
	return v, nil
}

// Peer returns the host address of a stack peer's port, e.g. Peer("cache",
// "6379") reads SANDBOX_CACHE_HOST and SANDBOX_CACHE_PORT_6379.
func Peer(service, containerPort string) (string, error) {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, service)
	host, port := os.Getenv("SANDBOX_"+name+"_HOST"), os.Getenv("SANDBOX_"+name+"_PORT_"+containerPort)
	if host == "" || port == "" {
		return "", fmt.Errorf("no peer %s exposing port %s in this stack", service, containerPort)
	}
	return host + ":" + port, nil
}

// IPCName returns the POSIX shared memory or semaphore name for name: under
// private IPC, the default, names must start with the container's prefix,
// which also keeps them apart from other containers'. macOS caps the names
// at 31 bytes.
func IPCName(name string) string {
	return os.Getenv("SANDBOX_IPC_PREFIX") + strings.TrimPrefix(name, "/")
}

// Become runs the rest of the process as user, as the official images'
// entrypoints do with gosu: a process the container runs as root first
// gives user the paths own, and everything under them. Under any other
// user, the container's choice stands and Become does nothing.
func Become(user string, own ...string) error {
	if os.Getuid() != 0 {
		return nil
	}
	id, err := identity.Resolve(Root(), user)
	if err != nil {
		return err
	}
	uid, gid := int(id.UID), int(id.GID)
	for _, p := range own {
		// A volume's mount point is a symlink to the volume.
		p, err := filepath.EvalSymlinks(p)
		if err != nil {
			return err
		}
		err = filepath.WalkDir(p, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Lchown(path, uid, gid)
		})
		if err != nil {
			return err
		}
	}
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return err
	}
	if err := syscall.Setgid(gid); err != nil {
		return err
	}
	if err := syscall.Setuid(uid); err != nil {
		return err
	}
	if id.Home != "" {
		os.Setenv("HOME", Path(id.Home))
	}
	os.Setenv("USER", user)
	return nil
}
