// Command entry runs a command in the fws Playwright image with Playwright
// set up: PLAYWRIGHT_BROWSERS_PATH names the browsers the image carries,
// NODE_PATH its playwright and @playwright/test packages, and
// NPM_CONFIG_PREFIX /usr, whose bin holds the playwright command, as host
// paths, which is how programs in a fwsd container read paths. npx looks
// for a command in the global prefix's bin before it fetches a package, so
// `npx playwright test` runs the image's runner; and the runner, reached
// through that prefix, and a spec's import of @playwright/test, reached
// through NODE_PATH, load one copy of the package, which the runner
// requires.
//
// The image's configuration, not this program, turns off Firefox's own
// sandboxes (MOZ_DISABLE_*_SANDBOX), so that a `docker exec`, which starts
// no entrypoint, has them off too: macOS refuses a second Seatbelt profile
// to a sandboxed process, and a Firefox child that fails to enter its own
// aborts. A launch that passes its own env must keep them.
package main

import (
	"log"
	"os"
	"strings"
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
	// The image's configuration names these as container paths, for a
	// docker exec, which starts no entrypoint; they are replaced, not
	// added to, as getenv returns a variable's first entry.
	set := map[string]string{
		"PLAYWRIGHT_BROWSERS_PATH": sandboxenv.Path("/ms-playwright"),
		"NODE_PATH":                sandboxenv.Path("/usr/lib/node_modules"),
		"NPM_CONFIG_PREFIX":        sandboxenv.Path("/usr"),
	}
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); set[k] == "" {
			env = append(env, kv)
		}
	}
	for k, v := range set {
		env = append(env, k+"="+v)
	}
	prog, err := execpath.Resolve(sandboxenv.Root(), args[0], env)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(prog, args, env))
}
