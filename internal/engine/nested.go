package engine

import (
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/e6qu/fun-with-sandboxes/internal/rootfs"
	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// A privileged container runs an engine of its own, as Docker's dind
// image runs dockerd in one: an engine nested in this one, with its own
// containers, networks, volumes and images, whose API this engine serves
// on a socket the container reaches. macOS refuses a second Seatbelt
// profile in a sandboxed process, so the container cannot confine its
// containers itself; this engine runs them, each in a sandbox of its own,
// as the nested engine's.

// Nested is what an engine nested in a privileged container takes from
// the engine that runs that container.
type Nested struct {
	// Library is the container library the outer engine built.
	Library string
	// RunDir holds the nested engine's sockets: under the outer engine's,
	// as macOS caps a socket's path at 104 bytes.
	RunDir string
	// StateRoot is the outermost engine's state, which no container reads.
	StateRoot string
	// Path resolves a path of the privileged container, through its
	// mounts, to the host's: the source of a bind mount.
	Path func(string) (string, error)
	// Port is where a port the nested engine publishes at the privileged
	// container's port p listens: the host port of that port, or, for a
	// port without one, the directory of the container's Unix sockets.
	Port func(p int) (hostPort int, dir string, err error)
}

// EngineVar gives a privileged container's processes the socket of its
// nested engine's API.
const EngineVar = "FWS_ENGINE"

// nestedBlocks is how many of the outer engine's port blocks a nested
// engine takes for its stacks.
const nestedBlocks = 4

type nestedEngine struct {
	e     *Engine
	serve io.Closer
}

func (e *Engine) runDir() string {
	if n := e.cfg.Nested; n != nil {
		return n.RunDir
	}
	return filepath.Join(e.cfg.Root, "run")
}

// StateRoot is the outermost engine's state, which no container reads.
func (e *Engine) StateRoot() string { return e.stateRoot() }

func (e *Engine) stateRoot() string {
	if n := e.cfg.Nested; n != nil {
		return n.StateRoot
	}
	return e.cfg.Root
}

// HostPath is the host's path for path p of the engine's host: p, or for
// an engine nested in a privileged container, the host's path for that
// container's p.
func (e *Engine) HostPath(p string) (string, error) { return e.hostPath(p) }

// hostPath is the host's path for the source of a bind mount.
func (e *Engine) hostPath(p string) (string, error) {
	if n := e.cfg.Nested; n != nil {
		return n.Path(p)
	}
	return filepath.Abs(p)
}

func (e *Engine) engineSocket(c *Container) string {
	return filepath.Join(e.runDir(), c.rec.ID[:12]+".engine")
}

func nestedStack(c *Container) string { return "nested-" + c.rec.ID[:12] }

// openNested opens the engine nested in privileged container c and serves
// its API, as the container starts or this engine finds it running.
func (e *Engine) openNested(c *Container) error {
	e.nestedMu.Lock()
	defer e.nestedMu.Unlock()
	if _, ok := e.nested[c.rec.ID]; ok {
		return nil
	}
	if e.cfg.Serve == nil {
		return unsupported("privileged mode runs an engine nested in the container, which this engine does not serve")
	}
	lib, err := e.Library()
	if err != nil {
		return err
	}
	base, limit, err := e.alloc.Reserve(nestedStack(c), nestedBlocks)
	if err != nil {
		return err
	}
	runDir := filepath.Join(e.runDir(), c.rec.ID[:12])
	sub, err := Open(Config{
		Root: filepath.Join(c.dir, "engine"), PortBase: base, PortLimit: limit, BlockSize: e.alloc.BlockSize(),
		Egress: e.cfg.Egress, LiveRestore: e.cfg.LiveRestore, Serve: e.cfg.Serve,
		Nested: &Nested{
			Library: lib, RunDir: runDir, StateRoot: e.stateRoot(),
			Path: func(p string) (string, error) { return e.pathIn(c, p) },
			Port: func(p int) (int, string, error) { return e.portIn(c, p) },
		},
	})
	if err != nil {
		return err
	}
	sock := e.engineSocket(c)
	os.Remove(sock)
	srv, err := e.cfg.Serve(sub, sock)
	if err != nil {
		sub.shutdown(true)
		return err
	}
	e.nested[c.rec.ID] = &nestedEngine{e: sub, serve: srv}
	return nil
}

// closeNested stops serving c's nested engine and ends its containers, as
// they end with the dockerd of a dind container.
func (e *Engine) closeNested(c *Container) {
	e.nestedMu.Lock()
	n := e.nested[c.rec.ID]
	delete(e.nested, c.rec.ID)
	e.nestedMu.Unlock()
	if n == nil {
		return
	}
	n.serve.Close()
	n.e.shutdown(true)
	os.Remove(e.engineSocket(c))
}

// removeNested frees what c's nested engine held outside c's directory.
func (e *Engine) removeNested(c *Container) error {
	e.closeNested(c)
	if err := os.RemoveAll(filepath.Join(e.runDir(), c.rec.ID[:12])); err != nil {
		return err
	}
	return e.alloc.Release(nestedStack(c))
}

// pathIn resolves p in container c as its processes see it: through the
// mount that holds it, else in its root.
func (e *Engine) pathIn(c *Container, p string) (string, error) {
	p = path.Clean("/" + p)
	c.mu.Lock()
	mounts := c.rec.Mounts
	root := c.root()
	c.mu.Unlock()
	best, bestLen := -1, -1
	for i, m := range mounts {
		d := path.Clean("/" + m.Destination)
		if (p == d || strings.HasPrefix(p, d+"/")) && len(d) > bestLen {
			best, bestLen = i, len(d)
		}
	}
	if best >= 0 {
		rel := strings.TrimPrefix(p, path.Clean("/"+mounts[best].Destination))
		if rel == "" {
			return mounts[best].Source, nil
		}
		return rootfs.Join(mounts[best].Source, rel)
	}
	return rootfs.Join(root, p)
}

// portIn is where c's port p listens: on its host port, or for a port
// without one, in the directory of its Unix sockets.
func (e *Engine) portIn(c *Container, p int) (int, string, error) {
	net, err := e.netOwner(c)
	if err != nil {
		return 0, "", err
	}
	if p != 0 {
		if hp, ok := net.rec.Ports[fmt.Sprintf("%d/tcp", p)]; ok {
			return hp, "", nil
		}
	}
	if net.rec.RouterPort == 0 {
		return 0, "", unsupported("the privileged container is on no network, so its engine's containers cannot publish ports in it")
	}
	return 0, e.portDir(net), nil
}

// DialPort connects to port p of container ref's network, where a process
// of the network listens on it: the port's host port when it has one, else
// the Unix socket the container library binds the port to.
func (e *Engine) DialPort(ref string, p int) (net.Conn, error) {
	c, err := e.Container(ref)
	if err != nil {
		return nil, err
	}
	hp, dir, err := e.portIn(c, p)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
	}
	return net.Dial("unix", router.SocketPath(dir, p))
}

// StreamPort is the port the engine serves the Container Runtime
// Interface's exec, attach and port-forward streams on in a privileged
// container, containerd's.
const StreamPort = 10010

// ListenStreams listens for the Container Runtime Interface's streams and
// returns the address its clients reach the listener at: a host loopback
// port, or for an engine nested in a privileged container, that
// container's StreamPort on its loopback.
func (e *Engine) ListenStreams() (net.Listener, string, error) {
	n := e.cfg.Nested
	if n == nil {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", err
		}
		return l, l.Addr().String(), nil
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(StreamPort))
	hp, dir, err := n.Port(StreamPort)
	if err != nil {
		return nil, "", err
	}
	if dir == "" {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
		return l, addr, err
	}
	path := router.SocketPath(dir, StreamPort)
	os.Remove(path)
	l, err := net.Listen("unix", path)
	return l, addr, err
}
