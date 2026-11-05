// Command entry runs MinIO inside a fwsd container as the official
// image's entrypoint does: arguments that do not start with minio run
// minio with them. The server listens on the container's port 9000, which
// the container library puts on its host port.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "health" {
		if err := live("http://127.0.0.1:9000/minio/health/live"); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	if len(args) == 0 {
		args = []string{"minio"}
	} else if args[0] != "minio" {
		args = append([]string{"minio"}, args...)
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(bin, args, os.Environ()))
}

// live asks MinIO's liveness endpoint, which answers 200 once it serves.
func live(url string) error {
	c := http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return nil
}
