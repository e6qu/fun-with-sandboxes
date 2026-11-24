// Command entry runs a command in the fws Playwright image. The image's
// configuration sets Playwright up, as container paths the container
// library maps, so a docker exec, which starts no entrypoint, finds the
// same: PLAYWRIGHT_BROWSERS_PATH names the browsers the image carries,
// NODE_PATH its playwright and @playwright/test packages, and
// NPM_CONFIG_PREFIX /usr/local, whose lib/node_modules holds them and
// whose bin the playwright command, so `npx playwright test` runs the
// image's runner. Not /usr: /usr/lib is a host tree, which the container
// library leaves to the host, so the packages there were in no container's
// view (fws-mkimage now refuses a file there). A host path in any of
// them would reach the shell npx runs the command with in its PATH, where
// it names no container path. The runner and a spec's import of
// @playwright/test resolve to one copy of the package, which the runner
// requires.
//
// The configuration also turns off Firefox's own sandboxes
// (MOZ_DISABLE_*_SANDBOX): macOS refuses a second Seatbelt profile to a
// sandboxed process, and a Firefox child that fails to enter its own
// aborts. A launch that passes its own env must keep them.
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
	prog, err := execpath.Resolve(sandboxenv.Root(), args[0], os.Environ())
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(prog, args, os.Environ()))
}
