// Package sandboxenv reads the variables the engine gives every container
// process: its root directory and the host ports its stack assigned.
package sandboxenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
