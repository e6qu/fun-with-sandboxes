// Command lambda-entrypoint starts a fws Lambda image's function, as the
// AWS base images' /lambda-entrypoint.sh does: under the Runtime Interface
// Emulator, on the ports the engine assigned to 8080 (invocations) and 9001
// (the Runtime API), or, where AWS_LAMBDA_RUNTIME_API names a Runtime API
// already, straight into the image's runtime. The handler is the last
// argument, the image's CMD.
package main

import (
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("lambda-entrypoint: ")
	invokePort, err := sandboxenv.Port("8080")
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "health" {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+invokePort, 2*time.Second)
		if err != nil {
			log.Print(err)
			os.Exit(1)
		}
		c.Close()
		return
	}
	handler := os.Getenv("_HANDLER")
	if len(os.Args) > 1 {
		handler = os.Args[len(os.Args)-1]
	}
	if handler == "" {
		log.Fatal("no handler: name it as the command, such as app.handler")
	}
	runtime, err := runtimeCommand(os.Getenv("FWS_LAMBDA_RUNTIME"))
	if err != nil {
		log.Fatal(err)
	}
	cmd := append(runtime, handler)
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		log.Fatal(syscall.Exec(cmd[0], cmd, os.Environ()))
	}
	apiPort, err := sandboxenv.Port("9001")
	if err != nil {
		log.Fatal(err)
	}
	rie, err := filepath.EvalSymlinks(sandboxenv.Path("/bin/aws-lambda-rie"))
	if err != nil {
		log.Fatal(err)
	}
	argv := append([]string{"aws-lambda-rie",
		"--runtime-interface-emulator-address", "127.0.0.1:" + invokePort,
		"--runtime-api-address", "127.0.0.1:" + apiPort,
	}, cmd...)
	log.Fatal(syscall.Exec(rie, argv, os.Environ()))
}

// runtimeCommand turns the image's runtime command, in container paths,
// into host paths: its program from the image's /bin, and every absolute
// argument under the container's root.
func runtimeCommand(spec string) ([]string, error) {
	words := strings.Fields(spec)
	if len(words) == 0 {
		return nil, os.ErrInvalid
	}
	prog, err := filepath.EvalSymlinks(sandboxenv.Path("/bin/" + words[0]))
	if err != nil {
		return nil, err
	}
	out := []string{prog}
	for _, w := range words[1:] {
		if strings.HasPrefix(w, "/") {
			w = sandboxenv.Path(w)
		}
		out = append(out, w)
	}
	return out, nil
}
