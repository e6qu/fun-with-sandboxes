// Command lambda-entrypoint starts a fws Lambda image's function, as the
// AWS base images' /lambda-entrypoint.sh does: the image's runtime under
// the Runtime Interface Emulator, on the ports the engine assigned to 8080
// (invocations) and 9001 (the Runtime API), or, where AWS_LAMBDA_RUNTIME_API
// names a Runtime API already, the runtime alone. The handler is the last
// argument, the image's CMD.
package main

import (
	"log"
	"net"
	"os"
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
	// The image's runtime is /var/runtime/bootstrap, as in AWS's base
	// images, which runs the function _HANDLER names.
	os.Setenv("_HANDLER", handler)
	const bootstrap = "/var/runtime/bootstrap"
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		log.Fatal(syscall.Exec(bootstrap, []string{bootstrap}, os.Environ()))
	}
	apiPort, err := sandboxenv.Port("9001")
	if err != nil {
		log.Fatal(err)
	}
	const rie = "/bin/aws-lambda-rie"
	argv := []string{"aws-lambda-rie",
		"--runtime-interface-emulator-address", "127.0.0.1:" + invokePort,
		"--runtime-api-address", "127.0.0.1:" + apiPort,
		bootstrap, handler,
	}
	log.Fatal(syscall.Exec(rie, argv, os.Environ()))
}
