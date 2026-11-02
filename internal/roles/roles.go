// Package roles names the binary that runs the engine's helper processes —
// the spawn helper, each container's supervisor, each stack's egress proxy
// — and the variable that tells that binary which role to take.
package roles

import (
	"os"
	"sync"
)

// Var is the environment variable whose value names the role.
const Var = "FWS_ROLE"

var (
	mu     sync.Mutex
	binary string
)

// Use makes path the binary that runs the helper roles. fwsd uses
// fws-shim, a binary that holds only those roles, so each helper process
// carries none of the engine's own weight.
func Use(path string) {
	mu.Lock()
	defer mu.Unlock()
	binary = path
}

// Binary is the binary that runs the helper roles: the one Use named, or
// the running binary, which holds every role its packages register, as a
// test binary does.
func Binary() (string, error) {
	mu.Lock()
	b := binary
	mu.Unlock()
	if b != "" {
		return b, nil
	}
	return os.Executable()
}
