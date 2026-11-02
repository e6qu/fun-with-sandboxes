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
	"github.com/e6qu/fun-with-sandboxes/internal/buildkit"
	"github.com/e6qu/fun-with-sandboxes/internal/engine"
	"github.com/e6qu/fun-with-sandboxes/internal/roles"
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
	egress := flag.Bool("egress", true, "give each stack an HTTP proxy to the internet that refuses destinations on this host")
	withBuildKit := flag.Bool("buildkit", true, "serve BuildKit for docker build, buildx and docker compose build")
	liveRestore := flag.Bool("live-restore", false, "keep containers running while the engine stops, and reconnect to them when it starts")
	flag.Parse()

	if os.Geteuid() == 0 {
		log.Fatal("fwsd must not run as root: it confines containers to the invoking user's rights")
	}
	if *socket == "" {
		*socket = filepath.Join(*root, "docker.sock")
	}
	helper, err := shimBinary()
	if err != nil {
		log.Fatal(err)
	}
	roles.Use(helper)
	cfg := engine.Config{Root: *root, PortBase: *portBase, PortLimit: *portLimit, BlockSize: *block, Egress: *egress, LiveRestore: *liveRestore}
	if err := run(cfg, *socket, *withBuildKit); err != nil {
		log.Fatal(err)
	}
}

// shimBinary is fws-shim, which runs every container's supervisor and the
// processes the engine starts; go install installs it beside fwsd.
func shimBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	p := filepath.Join(filepath.Dir(self), "fws-shim")
	if fi, err := os.Stat(p); err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("fws-shim must be installed beside fwsd, at %s: go install github.com/e6qu/fun-with-sandboxes/cmd/...", p)
	}
	return p, nil
}

func run(cfg engine.Config, socket string, withBuildKit bool) error {
	e, err := engine.Open(cfg)
	if err != nil {
		return err
	}
	var bk api.BuildKit
	if withBuildKit {
		svc, err := buildkit.New(context.Background(), buildkit.Config{
			Root: filepath.Join(e.Root(), "buildkit"), StateRoot: e.Root(), Images: e.Images(), Egress: e.BuildEgress,
		})
		if err != nil {
			return fmt.Errorf("start BuildKit: %w", err)
		}
		defer svc.Close()
		bk = svc
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
	srv := &http.Server{Handler: api.New(e, bk), ReadHeaderTimeout: 30 * time.Second}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	log.Printf("serving the Docker API on unix://%s (state in %s)", socket, e.Root())

	select {
	case err = <-served:
	case sig := <-sigs:
		if cfg.LiveRestore {
			log.Printf("%s: leaving containers running", sig)
		} else {
			log.Printf("%s: stopping containers", sig)
		}
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
