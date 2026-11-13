// Command entry runs a command in the fws Playwright image with Playwright
// set up: PLAYWRIGHT_BROWSERS_PATH names the browsers the image carries and
// NODE_PATH its playwright package, as host paths, which is how programs in
// a fwsd container read paths.
package main

import (
	"log"
	"os"
	"syscall"

	"github.com/e6qu/fun-with-sandboxes/internal/execpath"
	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"node"}
	}
	env := append(os.Environ(),
		"PLAYWRIGHT_BROWSERS_PATH="+sandboxenv.Path("/ms-playwright"),
		"NODE_PATH="+sandboxenv.Path("/usr/lib/node_modules"),
	)
	prog, err := execpath.Resolve(sandboxenv.Root(), args[0], env)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(prog, args, env))
}
