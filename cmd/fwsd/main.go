// Command fwsd serves the Docker Engine API on a unix socket and runs each
// container on the host under Seatbelt, without root and without a VM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/api"
	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	root := flag.String("root", filepath.Join(home, ".fws"), "engine state directory")
	socket := flag.String("socket", "", "unix socket to serve the Docker API on (default <root>/docker.sock)")
	portBase := flag.Int("port-base", 20000, "first host port handed to stacks")
	portLimit := flag.Int("port-limit", 30000, "end of the host port range handed to stacks (exclusive)")
	block := flag.Int("block-size", 100, "host ports reserved for each stack")
	flag.Parse()

	if os.Geteuid() == 0 {
		log.Fatal("fwsd must not run as root: it confines containers to the invoking user's rights")
	}
	if *socket == "" {
		*socket = filepath.Join(*root, "docker.sock")
	}
	if err := run(*root, *socket, *portBase, *portLimit, *block); err != nil {
		log.Fatal(err)
	}
}

func run(root, socket string, portBase, portLimit, block int) error {
	e, err := engine.Open(engine.Config{Root: root, PortBase: portBase, PortLimit: portLimit, BlockSize: block})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return err
	}
	if c, err := net.Dial("unix", socket); err == nil {
		c.Close()
		return fmt.Errorf("another engine is already serving %s", socket)
	}
	os.Remove(socket)
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		return err
	}
	srv := &http.Server{Handler: api.New(e), ReadHeaderTimeout: 30 * time.Second}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	log.Printf("serving the Docker API on unix://%s (state in %s)", socket, e.Root())

	select {
	case err = <-served:
	case sig := <-sigs:
		log.Printf("%s: stopping containers", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		srv.Shutdown(ctx)
		cancel()
	}
	e.Close()
	os.Remove(socket)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
