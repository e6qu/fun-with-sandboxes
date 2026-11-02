// Command fws-shim runs the engine's helper processes: the spawn helper
// every container process starts through, each container's supervisor,
// and each stack's egress proxy. fwsd starts it from its own directory;
// it holds only those roles, so a supervisor per container stays small.
package main

import (
	"fmt"
	"os"

	_ "github.com/e6qu/fun-with-sandboxes/internal/egress"
	_ "github.com/e6qu/fun-with-sandboxes/internal/runtime"
	_ "github.com/e6qu/fun-with-sandboxes/internal/shim"
)

// The roles' packages take over in their init functions when the role
// variable names them; main runs only when it names none.
func main() {
	fmt.Fprintln(os.Stderr, "fws-shim runs fwsd's helper processes; fwsd starts it")
	os.Exit(2)
}
