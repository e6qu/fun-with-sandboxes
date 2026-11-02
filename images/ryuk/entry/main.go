// Command entry starts Ryuk, Testcontainers' reaper, under the container
// contract: listening on the host port assigned to container port 8080, and
// reaching the engine through the socket Testcontainers mounts at
// /var/run/docker.sock.
package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

func main() {
	port, err := sandboxenv.Port("8080")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("RYUK_PORT", port)
	os.Setenv("DOCKER_HOST", "unix://"+sandboxenv.Path("/var/run/docker.sock"))
	ryuk := sandboxenv.Path("/ryuk")
	if err := syscall.Exec(ryuk, []string{ryuk}, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "exec ryuk:", err)
		os.Exit(1)
	}
}
