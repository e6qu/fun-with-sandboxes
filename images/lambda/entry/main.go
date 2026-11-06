// Command lambda-entrypoint starts a fws Lambda image's function, as the
// AWS base images' /lambda-entrypoint.sh does: the image's runtime under
// the Runtime Interface Emulator, which serves invocations on the
// container's port 8080 and the Runtime API on 9001, or, where
// AWS_LAMBDA_RUNTIME_API names a Runtime API already, the runtime alone. The
// handler is the last argument, the image's CMD.
package main

import (
	"log"
	"net"
	"os"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("lambda-entrypoint: ")
	if len(os.Args) > 1 && os.Args[1] == "health" {
		c, err := net.DialTimeout("tcp", "127.0.0.1:8080", 2*time.Second)
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
	const rie = "/bin/aws-lambda-rie"
	log.Fatal(syscall.Exec(rie, []string{"aws-lambda-rie", bootstrap, handler}, os.Environ()))
}
