// Command fwsd serves the Docker Engine API on a unix socket and runs each
// container on the host under Seatbelt, without root and without a VM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	archives := flag.String("image-archives", "", "a directory of image archives (*.tar, as docker save writes them) whose images load the first time they are asked for")
	debug := flag.Bool("debug", false, "log each API request, with its status and how long the engine took to serve it, and the steps of each container's create and start")
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
	cfg := engine.Config{Root: *root, PortBase: *portBase, PortLimit: *portLimit, BlockSize: *block, Egress: *egress, LiveRestore: *liveRestore, Debug: *debug, ImageArchives: *archives}
	if err := run(cfg, *socket, *withBuildKit, *debug); err != nil {
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

// startBuildKit starts BuildKit for an engine, which builds into its
// images.
func startBuildKit(e *engine.Engine) (*buildkit.Service, error) {
	svc, err := buildkit.New(context.Background(), buildkit.Config{
		Root: filepath.Join(e.Root(), "buildkit"), StateRoot: e.StateRoot(), Images: e.Images(), Egress: e.BuildEgress,
		Library: e.Library,
	})
	if err != nil {
		return nil, fmt.Errorf("start BuildKit: %w", err)
	}
	return svc, nil
}

// apiServer serves the engine's APIs: Docker's and Podman's over HTTP/1.1,
// and the Container Runtime Interface over HTTP/2 without TLS, which its
// gRPC clients speak to a Unix socket.
func apiServer(e *engine.Engine, bk api.BuildKit, debug bool) (*http.Server, *api.Server) {
	h := api.New(e, bk)
	return newHTTPServer(h, debug), h
}

func newHTTPServer(h http.Handler, debug bool) *http.Server {
	if debug {
		h = logRequests(h)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second, Protocols: new(http.Protocols)}
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)
	return srv
}

// nestedAPI serves the API of the engine nested in a privileged container
// on its socket, with BuildKit of its own when the engine has BuildKit.
type nestedAPI struct {
	srv *http.Server
	api *api.Server
	bk  *buildkit.Service
}

func (n *nestedAPI) Close() error {
	var err error
	if n.srv != nil {
		err = n.srv.Close()
		n.api.Close()
	}
	if n.bk != nil {
		n.bk.Close()
	}
	return err
}

func serveNested(withBuildKit, debug bool) func(*engine.Engine, string) (io.Closer, error) {
	return func(e *engine.Engine, socket string) (io.Closer, error) {
		n := &nestedAPI{}
		var bk api.BuildKit
		if withBuildKit {
			svc, err := startBuildKit(e)
			if err != nil {
				return nil, err
			}
			n.bk, bk = svc, svc
		}
		l, err := net.Listen("unix", socket)
		if err != nil {
			n.Close()
			return nil, err
		}
		n.srv, n.api = apiServer(e, bk, debug)
		go n.srv.Serve(l)
		return n, nil
	}
}

func run(cfg engine.Config, socket string, withBuildKit, debug bool) error {
	cfg.Serve = serveNested(withBuildKit, debug)
	e, err := engine.Open(cfg)
	if err != nil {
		return err
	}
	// The container library and the anchor launcher compile with clang the
	// first time an engine needs them, which takes seconds; they compile
	// now, as the engine starts, rather than in its first container's start.
	go func() {
		if _, err := e.Library(); err != nil {
			log.Printf("build the container library: %v", err)
		}
	}()
	var bk api.BuildKit
	if withBuildKit {
		svc, err := startBuildKit(e)
		if err != nil {
			return err
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
	srv, h := apiServer(e, bk, debug)
	defer h.Close()

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
