// Command entry runs DynamoDB Local inside a fwsd container, storing its
// tables under /home/dynamodblocal/data as the official image's working
// directory does. The server listens on the container's port 8000, which the
// container library puts on its host port.
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
	if len(os.Args) > 1 && os.Args[1] == "health" {
		// DynamoDB Local answers any request, an unsigned one with an
		// error: an answer means it serves.
		c := http.Client{Timeout: 2 * time.Second}
		res, err := c.Get("http://127.0.0.1:8000/")
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
		"-jar", filepath.Join(home, "DynamoDBLocal.jar"), "-port", "8000"}
	log.Fatal(syscall.Exec(java, append(argv, args...), os.Environ()))
}
