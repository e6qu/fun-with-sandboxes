package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/e6qu/fun-with-sandboxes/internal/alloc"
	stackmembers "github.com/e6qu/fun-with-sandboxes/internal/members"
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
	// container's port p, TCP or UDP by proto, listens: the host port of
	// that port, or, for a port without one, the directory of the
	// container's Unix sockets.
	Port func(proto string, p int) (hostPort int, dir string, err error)
	// Dial connects to a port of a member the privileged container
	// shares a network with, at its address on that network: how a
	// nested engine's service reaches an endpoint beyond its own
	// members, such as a cluster's API server.
	Dial func(netip.AddrPort) (net.Conn, error)
	// DialUDP opens a UDP flow to such a member's port, as Dial connects:
	// how a nested engine's UDP Service reaches an endpoint on another
	// node, such as cluster DNS's.
	DialUDP func(netip.AddrPort) (net.Conn, error)
	// Resolve finds the address of the member that answers to name on a
	// network the privileged container is on, as its processes resolve
	// it: how the nested engine dials a registry by its name there.
	Resolve func(name string) (netip.Addr, error)
	// Publish hands the engine it is nested in this engine's members on
	// networks with subnets of their own, as they change; Peers are those
	// the engines nested in the containers its container shares a network
	// with published (peers.go).
	Publish func([]stackmembers.Member)
	Peers   func() []stackmembers.Member
	// Node is the engine that runs the privileged container, and the
	// container: the network of a container on the host's network mode,
	// as a kubelet's static pods run on their node's.
	Node func() (*Engine, *Container)
}

// EngineVar gives a privileged container's processes the socket of its
// nested engine's API.
const EngineVar = "FWS_ENGINE"

// nestedBlocks is how many of the outer engine's port blocks a nested
// engine takes for its stacks.
const nestedBlocks = 4

// nestedStacks is how many stacks a nested engine's range holds: a
// kubelet's node makes one for each pod's sandbox, which keeps it until the
// pod is gone, so the outer engine's blocks are cut smaller. A stack's
// ports hold only what it publishes and its UDP flows' sources, as the
// ports its members listen on are Unix sockets.
const nestedStacks = 40

// nestedBlockSize is the block each of a nested engine's stacks holds, for
// an outer block of size.
func nestedBlockSize(size int) int {
	return max(1, size*nestedBlocks/nestedStacks)
}

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
		Root: filepath.Join(c.dir, "engine"), PortBase: base, PortLimit: limit, BlockSize: nestedBlockSize(e.alloc.BlockSize()),
		Egress: e.cfg.Egress, LiveRestore: e.cfg.LiveRestore, Serve: e.cfg.Serve,
		Nested: &Nested{
			Library: lib, RunDir: runDir, StateRoot: e.stateRoot(),
			Path:    func(p string) (string, error) { return e.pathIn(c, p) },
			Port:    func(proto string, p int) (int, string, error) { return e.portIn(c, proto, p) },
			Dial:    func(ap netip.AddrPort) (net.Conn, error) { return e.dialFrom(c, ap) },
			DialUDP: func(ap netip.AddrPort) (net.Conn, error) { return e.dialFromUDP(c, ap) },
			Resolve: func(name string) (netip.Addr, error) { return e.resolveFrom(c, name) },
			Publish: func(ms []stackmembers.Member) { e.publishPeer(c, ms) },
			Peers:   func() []stackmembers.Member { return e.peersOf(c.rec.ID) },
			Node:    func() (*Engine, *Container) { return e, c },
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
	e.nodes.refreshing.Go(func() { e.refreshPeers("") })
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
	e.dropPeer(c)
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

// portIn is where c's port p, TCP or UDP by proto, listens: on its host
// port, or for a port without one, in the directory of its Unix sockets.
func (e *Engine) portIn(c *Container, proto string, p int) (int, string, error) {
	net, err := e.netOwner(c)
	if err != nil {
		return 0, "", err
	}
	if p != 0 {
		if hp, ok := net.rec.Ports[fmt.Sprintf("%d/%s", p, proto)]; ok {
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
	hp, dir, err := e.portIn(c, "tcp", p)
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
	if e.cfg.Nested == nil {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", err
		}
		return l, l.Addr().String(), nil
	}
	l, err := e.listenOnNode(StreamPort)
	return l, net.JoinHostPort("127.0.0.1", strconv.Itoa(StreamPort)), err
}

// listenOnNode listens on port p of the engine's node: for an engine
// nested in a privileged container, that container's port p, where the
// members of its networks reach it; else port p of the host's loopback.
func (e *Engine) listenOnNode(p int) (net.Listener, error) {
	n := e.cfg.Nested
	if n == nil {
		return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	}
	hp, dir, err := n.Port("tcp", p)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
	}
	path := router.SocketPath(dir, p)
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The router writes each client's address before its stream, which
	// the container library strips for a container's own listeners.
	return router.Listener(l), nil
}

// dialFromUDP opens a UDP flow to port ap.Port of the member at ap.Addr
// that container c shares a network with, from a port of the member's
// stack's block, which its sandbox lets it answer: to its UDP host port, or
// its router's flow port for one without.
func (e *Engine) dialFromUDP(c *Container, ap netip.AddrPort) (net.Conn, error) {
	owner, err := e.netOwner(c)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	var hp, rp int
	var stack string
	found := false
	for _, o := range e.containers {
		if !o.isRunning() || !onNetwork(o) {
			continue
		}
		for n, es := range o.rec.Networks {
			if n == "none" || n == "host" || owner.rec.Networks[n] == nil || es == nil {
				continue
			}
			if es.IPAddress == ap.Addr() || (o.rec.Address == ap.Addr().String()) {
				hp, rp, stack, found = o.rec.Ports[fmt.Sprintf("%d/udp", ap.Port())], o.rec.RouterPort, o.rec.Stack, true
			}
		}
		if found {
			break
		}
	}
	e.mu.Unlock()
	if !found {
		// A pod of another node: another nested engine's member.
		return e.dialPeerUDP(owner, ap)
	}
	up, err := udpUpstream(hp, rp, ap)
	if err != nil {
		return nil, err
	}
	return alloc.DialFlow(e.cfg.Root, stack, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: up})
}

// udpUpstream is where a member's UDP port ap.Port receives: its host
// port hp, or the flow port its router at rp gives a port without one.
func udpUpstream(hp, rp int, ap netip.AddrPort) (int, error) {
	if hp != 0 {
		return hp, nil
	}
	if rp != 0 {
		return router.LookupUDP(net.JoinHostPort("127.0.0.1", strconv.Itoa(rp)), int(ap.Port()))
	}
	return 0, fmt.Errorf("%s does not listen on UDP port %d", ap.Addr(), ap.Port())
}

// dialFrom connects to port ap.Port of the member at ap.Addr that
// container c shares a network with: at its host port, or through its
// router.
func (e *Engine) dialFrom(c *Container, ap netip.AddrPort) (net.Conn, error) {
	owner, err := e.netOwner(c)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	var hp, rp int
	found := false
	for _, o := range e.containers {
		if !o.isRunning() || !onNetwork(o) {
			continue
		}
		for n, es := range o.rec.Networks {
			if n == "none" || n == "host" || owner.rec.Networks[n] == nil || es == nil {
				continue
			}
			if es.IPAddress == ap.Addr() || (o.rec.Address == ap.Addr().String()) {
				hp, rp, found = o.rec.Ports[fmt.Sprintf("%d/tcp", ap.Port())], o.rec.RouterPort, true
			}
		}
		if found {
			break
		}
	}
	e.mu.Unlock()
	switch {
	case !found:
		// A pod of another node: another nested engine's member.
		return e.dialPeer(owner, ap)
	case hp != 0:
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
	case rp != 0:
		return router.Dial(net.JoinHostPort("127.0.0.1", strconv.Itoa(rp)), int(ap.Port()))
	}
	return nil, fmt.Errorf("%s does not listen on port %d", ap.Addr(), ap.Port())
}

// resolveFrom finds the address of the member that answers to name on a
// user-defined network container c is on: its address there, or its
// member address on a network without a subnet.
func (e *Engine) resolveFrom(c *Container, name string) (netip.Addr, error) {
	owner, err := e.netOwner(c)
	if err != nil {
		return netip.Addr{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, n := range userNetworks(owner) {
		o := e.answeringLocked(n, name, owner)
		if o == nil {
			continue
		}
		if es := o.rec.Networks[n]; es != nil && es.IPAddress.IsValid() {
			return es.IPAddress, nil
		}
		if a, err := netip.ParseAddr(o.rec.Address); err == nil {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("no member answers to %s on %s's networks", name, c.rec.Name)
}

// NodeFile reads the file at path in the privileged container a nested
// engine runs in, as its processes find it; an engine nested in none has
// no node, and no such file.
func (e *Engine) NodeFile(path string) ([]byte, error) {
	n := e.cfg.Nested
	if n == nil || n.Path == nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	}
	p, err := n.Path(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// DialNode connects to addr, a host and port, as a process of the
// privileged container a nested engine runs in reaches it: a member of a
// network the container is on, by its address or its name there, through
// the engine the container runs in. Any other host, loopback included,
// which names the host's, and every host for an engine nested in none, is
// dialled from the host, as the engine pulls.
func (e *Engine) DialNode(ctx context.Context, network, addr string) (net.Conn, error) {
	n := e.cfg.Nested
	host, port, err := net.SplitHostPort(addr)
	if n == nil || n.Dial == nil || err != nil || (network != "tcp" && network != "tcp4" && network != "tcp6") {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("port %q: %v", port, err)
	}
	a, err := netip.ParseAddr(host)
	if err != nil && n.Resolve != nil {
		if r, rerr := n.Resolve(host); rerr == nil {
			a, err = r, nil
		}
	}
	if err != nil || a.IsLoopback() {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return n.Dial(netip.AddrPortFrom(a, uint16(p)))
}
