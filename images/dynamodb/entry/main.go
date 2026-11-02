// Command entry runs DynamoDB Local inside a fwsd container on the port the
// engine assigned to 8000, storing its tables under /home/dynamodblocal/data
// as the official image's working directory does, through a launcher that
// binds it to loopback.
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	port, err := sandboxenv.Port("8000")
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "health" {
		// DynamoDB Local answers any request, an unsigned one with an
		// error: an answer means it serves.
		c := http.Client{Timeout: 2 * time.Second}
		res, err := c.Get("http://127.0.0.1:" + port + "/")
		if err != nil {
			log.Print(err)
			os.Exit(1)
		}
		res.Body.Close()
		return
	}
	java, err := filepath.EvalSymlinks(sandboxenv.Path("/bin/java"))
	if err != nil {
		log.Fatal(err)
	}
	home := sandboxenv.Path(os.Getenv("DYNAMODB_HOME"))
	data := sandboxenv.Path("/home/dynamodblocal/data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		log.Fatal(err)
	}
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"-sharedDb", "-dbPath", data}
	}
	argv := []string{"java",
		"-Djava.library.path=" + filepath.Join(home, "DynamoDBLocal_lib"),
		"-Djava.io.tmpdir=" + sandboxenv.Path("/tmp"),
		// Java listens on an IPv6 socket that also takes IPv4 unless told
		// otherwise; the container may listen on its IPv4 loopback port.
		"-Djava.net.preferIPv4Stack=true",
		"-cp", filepath.Join(home, "DynamoDBLocal.jar") + ":" + sandboxenv.Path("/opt/fws"),
		"Launcher", "-port", port}
	log.Fatal(syscall.Exec(java, append(argv, args...), os.Environ()))
}
