// Package engine holds the containers, volumes and networks and runs their
// lifecycles: a container is an APFS clone of its image root, confined by
// Seatbelt, listening on host ports its stack owns.
package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/alloc"
	"github.com/e6qu/fun-with-sandboxes/internal/image"
	"github.com/e6qu/fun-with-sandboxes/internal/pathmap"
)

const (
	LabelProject = "com.docker.compose.project"
	LabelService = "com.docker.compose.service"
)

type Config struct {
	// Root is the engine's state directory.
	Root string
	// PortBase, PortLimit and BlockSize bound the host ports stacks receive.
	PortBase, PortLimit, BlockSize int
	// Egress gives each stack an HTTP proxy to the internet that refuses
	// destinations on the host.
	Egress bool
	// LiveRestore keeps containers running when the engine stops, as
	// dockerd's live-restore does; the next engine reconnects to them.
	LiveRestore bool
	// Debug logs how long each step of a container's create, start and
	// removal takes.
	Debug bool
	// ImageArchives, where set, is a directory of image archives the image
	// store loads from as their images are first asked for.
	ImageArchives string
	// MachineGuard pauses the container using the most memory while macOS
	// reports critical memory pressure, and the one that wrote the most
	// while the engine's disk is nearly full, and resumes them once the
	// measure is back to normal (guard.go).
	MachineGuard bool
	// Serve serves an engine's API on a Unix socket: the API of the engine
	// nested in each privileged container, which it serves until closed.
	Serve func(e *Engine, socket string) (io.Closer, error)
	// Nested makes this engine one nested in a privileged container.
	Nested *Nested
}

type Engine struct {
	cfg     Config
	alloc   *alloc.Allocator
	images  *image.Store
	events  *eventBus
	user    *user.User
	started time.Time

	egressMu sync.Mutex
	egress   map[string]*egressProc

	// pidnsMu orders changes to PID namespaces' files.
	pidnsMu sync.Mutex

	// detach closes when the engine leaves its containers running as it
	// stops.
	detach chan struct{}
	// closing closes as the engine shuts down, stopping its guards.
	closing     chan struct{}
	closingOnce sync.Once

	libOnce sync.Once
	lib     string
	libErr  error

	// buildMu is held shared by classic builds and exclusively by a prune of
	// their cache.
	buildMu sync.RWMutex

	nestedMu sync.Mutex
	nested   map[string]*nestedEngine

	// svcs are the services on the networks (services.go).
	svcs services
	// nodes are the members the engines nested in this one publish, and
	// published what this one, nested, last published (peers.go).
	nodes     nodePeers
	published []byte

	mu         timedMutex
	containers map[string]*Container
	volumes    map[string]*Volume
	networks   map[string]*Network
	execs      map[string]*Exec
}

func Open(cfg Config) (*Engine, error) {
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	// Seatbelt matches resolved paths, so every path the engine hands a
	// profile must be free of symlinks such as /var -> /private/var.
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, err
	}
	cfg.Root = root
	// Files an earlier engine's containers and images recorded an owner or
	// a mode on lack the flag the container library now reads first.
	marked := filepath.Join(root, ".meta-flags")
	if _, err := os.Stat(marked); errors.Is(err, fs.ErrNotExist) {
		if err := image.MarkTree(root); err != nil {
			return nil, fmt.Errorf("mark %s: %w", root, err)
		}
		if err := os.WriteFile(marked, nil, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err := clearScratch(root); err != nil {
		return nil, err
	}
	a, err := alloc.Open(alloc.Config{Root: root, PortBase: cfg.PortBase, PortLimit: cfg.PortLimit, BlockSize: cfg.BlockSize})
	if err != nil {
		return nil, err
	}
	imgs, err := image.Open(filepath.Join(root, "images"))
	if err != nil {
		return nil, err
	}
	if cfg.ImageArchives != "" {
		if err := imgs.UseArchives(cfg.ImageArchives); err != nil {
			return nil, err
		}
	}
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	e := &Engine{
		cfg: cfg, alloc: a, images: imgs, events: newEventBus(), user: u, started: time.Now(),
		egress:     map[string]*egressProc{},
		nested:     map[string]*nestedEngine{},
		detach:     make(chan struct{}),
		closing:    make(chan struct{}),
		containers: map[string]*Container{},
		volumes:    map[string]*Volume{},
		networks:   map[string]*Network{},
		execs:      map[string]*Exec{},
	}
	e.mu.timed = cfg.Debug
	if cfg.Nested != nil {
		e.libOnce.Do(func() { e.lib = cfg.Nested.Library })
	}
	// An engine nested in a container leaves the guard to the engine on the
	// host, whose container it runs in.
	if cfg.MachineGuard && cfg.Nested == nil {
		go e.guardMachine(machineGuards(root), e.closing)
	}
	if err := os.MkdirAll(e.runDir(), 0o700); err != nil {
		return nil, err
	}
	if err := e.loadVolumes(); err != nil {
		return nil, err
	}
	if err := e.loadNetworks(); err != nil {
		return nil, err
	}
	if err := e.loadContainers(); err != nil {
		return nil, err
	}
	// A privileged container this engine found running, under live
	// restore, gets its nested engine back.
	for _, c := range e.allContainers() {
		if c.rec.HostConfig.Privileged && c.isRunning() {
			if err := e.openNested(c); err != nil {
				return nil, err
			}
		}
	}
	return e, nil
}

func (e *Engine) Root() string { return e.cfg.Root }

// Library is the container library every container process loads, built
// on first use, as only a host that runs containers needs a C compiler.
func (e *Engine) Library() (string, error) {
	e.libOnce.Do(func() { e.lib, e.libErr = pathmap.Build(filepath.Join(e.cfg.Root, "lib")) })
	return e.lib, e.libErr
}
func (e *Engine) Images() *image.Store    { return e.images }
func (e *Engine) Alloc() *alloc.Allocator { return e.alloc }

// Close stops every running container, as dockerd does when it shuts down
// without live restore; with it, Close leaves the containers and their
// stacks' egress proxies running for the next engine.
func (e *Engine) Close() { e.shutdown(!e.cfg.LiveRestore) }

// shutdown stops the containers, or with live restore leaves them and
// their nested engines running for the next engine.
func (e *Engine) shutdown(stop bool) {
	e.closingOnce.Do(func() { close(e.closing) })
	if !stop {
		e.nestedMu.Lock()
		for _, n := range e.nested {
			n.serve.Close()
			n.e.shutdown(false)
		}
		e.nestedMu.Unlock()
		close(e.detach)
		return
	}
	var wg sync.WaitGroup
	for _, c := range e.allContainers() {
		wg.Add(1)
		go func(c *Container) {
			defer wg.Done()
			c.stopWith(e, nil, "")
		}(c)
	}
	wg.Wait()
	e.closeAllEgress()
	e.closeServices()
}

func newID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

var validName = regexp.MustCompile(`^/?[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

func (e *Engine) allContainers() []*Container {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Container, 0, len(e.containers))
	for _, c := range e.containers {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rec.Created.After(out[j].rec.Created) })
	return out
}

// Container resolves a full ID, a name, or an unambiguous ID prefix.
func (e *Engine) Container(ref string) (*Container, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.containerLocked(ref)
}

func (e *Engine) containerLocked(ref string) (*Container, error) {
	if c, ok := e.containers[ref]; ok {
		return c, nil
	}
	name := strings.TrimPrefix(ref, "/")
	for _, c := range e.containers {
		if c.rec.Name == name {
			return c, nil
		}
	}
	if _, child, _, ok := e.linkLocked(name); ok {
		return child, nil
	}
	var match *Container
	if len(ref) > 0 {
		for id, c := range e.containers {
			if strings.HasPrefix(id, ref) {
				if match != nil {
					return nil, invalid("multiple containers match %q", ref)
				}
				match = c
			}
		}
	}
	if match == nil {
		return nil, notFound("No such container: %s", ref)
	}
	return match, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

func timeString(t time.Time) string {
	if t.IsZero() {
		return "0001-01-01T00:00:00Z"
	}
	return t.UTC().Format(time.RFC3339Nano)
}
