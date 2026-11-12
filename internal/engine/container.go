package engine

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/egress"
	"github.com/e6qu/fun-with-sandboxes/internal/identity"
	"github.com/e6qu/fun-with-sandboxes/internal/image"
	"github.com/e6qu/fun-with-sandboxes/internal/pathmap"
	"github.com/e6qu/fun-with-sandboxes/internal/portproxy"
	"github.com/e6qu/fun-with-sandboxes/internal/procs"
	"github.com/e6qu/fun-with-sandboxes/internal/roles"
	"github.com/e6qu/fun-with-sandboxes/internal/rootfs"
	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
	"github.com/e6qu/fun-with-sandboxes/internal/shim"
)

const (
	LabelContainerNumber = "com.docker.compose.container-number"
	defaultStopTimeout   = 10 * time.Second
	// outputDrain bounds how long the engine reads output after a process
	// group is gone, for descendants that escaped the group.
	outputDrain = 2 * time.Second
)

type mountRec struct {
	Type mount.Type `json:"type"`
	// Name is a volume's name, or the ID of the image an image mount shows.
	Name        string `json:"name,omitempty"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	RW          bool   `json:"rw"`
	// Subpath is the directory of the volume or image the mount shows.
	Subpath string `json:"subpath,omitempty"`
	// NoCopy keeps a new volume empty rather than copying the image's
	// content at the mount point into it.
	NoCopy bool `json:"noCopy,omitempty"`
	// volumeOpts and volumeLabels are the driver options and labels a mount
	// gives a volume it creates.
	volumeOpts, volumeLabels map[string]string
}

type stateRec struct {
	Status       container.ContainerState `json:"status"`
	ExitCode     int                      `json:"exitCode"`
	Error        string                   `json:"error,omitempty"`
	StartedAt    time.Time                `json:"startedAt"`
	FinishedAt   time.Time                `json:"finishedAt"`
	Pid          int                      `json:"pid"`
	RestartCount int                      `json:"restartCount"`
	ManualStop   bool                     `json:"manualStop"`
	OOMKilled    bool                     `json:"oomKilled,omitempty"`
	Paused       bool                     `json:"paused,omitempty"`
	// Health is kept with the state, so an engine that reconnects to a
	// running container carries its health on.
	Health *container.Health `json:"health,omitempty"`
}

type record struct {
	ID         string                               `json:"id"`
	Name       string                               `json:"name"`
	Created    time.Time                            `json:"created"`
	Image      string                               `json:"image"`
	ImageID    string                               `json:"imageId"`
	Config     container.Config                     `json:"config"`
	HostConfig container.HostConfig                 `json:"hostConfig"`
	Networks   map[string]*network.EndpointSettings `json:"networks"`
	Stack      string                               `json:"stack"`
	Service    string                               `json:"service"`
	// Ports maps each container port ("5432/tcp") to the host port the stack
	// assigned it; the service listens there.
	Ports map[string]int `json:"ports"`
	// Published holds the host ports bound for the current run.
	Published network.PortMap `json:"published"`
	// Address is the container's member address in its stack, which the
	// container library presents its stack peers at.
	Address string `json:"address,omitempty"`
	// RouterPort is where the container's shim routes connections to its
	// TCP ports that have no host port (package router).
	RouterPort int        `json:"routerPort,omitempty"`
	Mounts     []mountRec `json:"mounts"`
	State      stateRec   `json:"state"`
	Links      []linkRec  `json:"links,omitempty"`
}

type Container struct {
	mu sync.Mutex
	// nameMu guards rec.Name for readers holding neither e.mu nor mu, such
	// as event publishing; rename takes it inside both. It guards nothing
	// else, so holding it never waits on another lock.
	nameMu sync.RWMutex
	rec    record
	dir    string
	logs   *logStore

	// task is the shim of the current run.
	task    *shim.Client
	runDone chan struct{}
	// nextRun closes when the next run starts.
	nextRun    chan struct{}
	removed    chan struct{}
	healthStop chan struct{}
	restarting *time.Timer
	starting   bool
}

func (c *Container) ID() string { return c.rec.ID }
func (c *Container) Name() string {
	c.nameMu.RLock()
	defer c.nameMu.RUnlock()
	return c.rec.Name
}
func (c *Container) root() string { return filepath.Join(c.dir, "r") }

// tag is the environment entry every process of the container carries.
func (c *Container) tag() string { return "SANDBOX_ROOT=" + c.root() }

func (c *Container) isRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rec.State.Status == container.StateRunning
}

func (c *Container) save() error { return writeJSON(filepath.Join(c.dir, "config.json"), c.rec) }

func (e *Engine) loadContainers() error {
	matches, err := filepath.Glob(filepath.Join(e.cfg.Root, "stacks", "*", "c", "*", "config.json"))
	if err != nil {
		return err
	}
	var restart []*Container
	for _, m := range matches {
		c := &Container{dir: filepath.Dir(m), removed: make(chan struct{}), runDone: closedChan(), nextRun: make(chan struct{})}
		if _, err := readJSON(m, &c.rec); err != nil {
			return err
		}
		if c.logs, err = openLogStore(filepath.Join(c.dir, "log.jsonl")); err != nil {
			return err
		}
		if c.rec.State.Status == container.StateRunning {
			if task, err := shim.Connect(e.shimSocket(c)); err == nil {
				if e.cfg.LiveRestore {
					// The container ran on while no engine did: follow it
					// again.
					c.mu.Lock()
					done := c.attachTaskLocked(e, task, true)
					c.mu.Unlock()
					go e.watch(c, task, done)
					e.containers[c.rec.ID] = c
					continue
				}
				// Without live restore an engine ends the containers an
				// earlier one left running, as dockerd does.
				task.Signal(syscall.SIGKILL)
				exit, err := task.Wait(nil)
				if err != nil {
					exit = e.lostExit(c)
				}
				c.recordExit(exit)
			} else {
				c.recordExit(e.lostExit(c))
			}
		}
		if c.rec.State.Status == container.StateRunning || c.rec.State.Status == container.StateRestarting {
			c.recordExit(shim.Exit{ExitCode: c.rec.State.ExitCode, FinishedAt: time.Now().UTC()})
		}
		// dockerd starts these as it starts, unless stopped by hand.
		if c.rec.State.Status == container.StateExited && !c.rec.State.ManualStop {
			if p := c.rec.HostConfig.RestartPolicy.Name; p == container.RestartPolicyAlways || p == container.RestartPolicyUnlessStopped {
				restart = append(restart, c)
			}
		}
		if err := c.save(); err != nil {
			return err
		}
		e.containers[c.rec.ID] = c
	}
	// Containers an engine made before member addresses existed get them.
	stacks := map[string]bool{}
	for _, c := range e.containers {
		stacks[c.rec.Stack] = true
		if c.rec.Address != "" || !onNetwork(c) {
			continue
		}
		a, err := e.assignAddressLocked(c.rec.Stack)
		if err != nil {
			return err
		}
		c.rec.Address = a
		if err := c.save(); err != nil {
			return err
		}
	}
	// And routers.
	for _, c := range e.containers {
		if c.rec.RouterPort != 0 || !onNetwork(c) {
			continue
		}
		key := c.rec.Service
		if n := c.rec.Config.Labels[LabelContainerNumber]; n != "" {
			key += "#" + n
		}
		p, err := e.alloc.Port(c.rec.Stack, key+"/router")
		if err != nil {
			return err
		}
		c.rec.RouterPort = p
		if err := c.save(); err != nil {
			return err
		}
	}
	for s := range stacks {
		if err := e.writeNetworkLocked(s); err != nil {
			return err
		}
	}
	for _, c := range restart {
		if err := c.start(e); err != nil {
			c.logs.append("stderr", fmt.Sprintf("restart after engine start failed: %v\n", err))
		}
	}
	return nil
}

// recordExit records how a run ended that no engine watched to its end.
func (c *Container) recordExit(exit shim.Exit) {
	c.rec.State.Status = container.StateExited
	c.rec.State.ExitCode = exit.ExitCode
	c.rec.State.OOMKilled = exit.OOMKilled
	c.rec.State.FinishedAt = exit.FinishedAt
	c.rec.State.Pid = 0
	c.rec.State.Paused = false
	c.rec.Published = nil
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// CreateOptions carries the query parameters of POST /containers/create.
type CreateOptions struct {
	Name     string
	Platform string
}

func (e *Engine) Create(opts CreateOptions, req container.CreateRequest) (container.CreateResponse, error) {
	if req.Config == nil {
		return container.CreateResponse{}, invalid("config cannot be empty in order to create a container")
	}
	if req.HostConfig == nil {
		req.HostConfig = &container.HostConfig{}
	}
	if opts.Platform != "" && opts.Platform != image.Platform.OS+"/"+image.Platform.Architecture {
		return container.CreateResponse{}, unsupported("requested platform %s does not match the engine platform %s/%s", opts.Platform, image.Platform.OS, image.Platform.Architecture)
	}
	if err := checkHostConfig(req.HostConfig); err != nil {
		return container.CreateResponse{}, err
	}
	// A nested engine puts a container on the host's network mode on the
	// privileged container's network (below); another refuses the mode.
	if _, node := e.hostNode(); req.HostConfig.NetworkMode == "host" && node == nil {
		return container.CreateResponse{}, unsupported("network mode \"host\" is not supported: every container is confined to its own stack's ports")
	}
	switch {
	case req.HostConfig.ShmSize < 0:
		return container.CreateResponse{}, invalid("SHM size can not be less than 0")
	case req.HostConfig.ShmSize == 0:
		req.HostConfig.ShmSize = defaultShmSize
	}
	// A container whose networking is disabled is on no network, as one in
	// network mode none.
	if req.Config.NetworkDisabled {
		req.HostConfig.NetworkMode = "none"
	}
	if req.HostConfig.UTSMode == "host" && req.Config.Hostname != "" {
		return container.CreateResponse{}, invalid("conflicting options: hostname and the UTS mode")
	}
	if err := checkContainerNetwork(req); err != nil {
		return container.CreateResponse{}, err
	}
	if err := e.checkUser(req.Config.User); err != nil {
		return container.CreateResponse{}, err
	}
	img, err := e.images.Get(req.Config.Image)
	if err != nil {
		return container.CreateResponse{}, notFound("No such image: %s", req.Config.Image)
	}
	cfg := mergeConfig(img, req.Config)
	if err := e.checkUser(cfg.User); err != nil {
		return container.CreateResponse{}, err
	}

	id := newID()
	name := strings.TrimPrefix(opts.Name, "/")
	if name == "" {
		name = generateName(id)
	} else if !validName.MatchString(name) {
		return container.CreateResponse{}, invalid("Invalid container name (%s), only [a-zA-Z0-9][a-zA-Z0-9_.-] are allowed", name)
	}

	stack := cfg.Labels[LabelProject]
	if stack == "" {
		stack = "run-" + id[:12]
	}
	service := cfg.Labels[LabelService]
	if service == "" {
		service = name
	}
	portKey := service
	if n := cfg.Labels[LabelContainerNumber]; n != "" {
		portKey += "#" + n
	}

	st := e.steps("create " + id[:12])
	defer st.log()
	staged, err := os.MkdirTemp(stagingDir(e.cfg.Root), id[:12]+"-")
	if err != nil {
		return container.CreateResponse{}, err
	}
	defer os.RemoveAll(staged)
	// The root and the record of its initial state, which create completes
	// with what it writes into the root.
	stagedRoot, stagedInit := filepath.Join(staged, "r"), filepath.Join(staged, "init")
	initDone := make(chan error, 1)
	go func() { initDone <- e.images.Clone(img.ID, stagedInit) }()
	err = e.images.Clone(img.ID, stagedRoot)
	if initErr := <-initDone; err == nil && initErr != nil {
		err = fmt.Errorf("record the initial root: %w", initErr)
	}
	if err != nil {
		return container.CreateResponse{}, fmt.Errorf("clone image root: %w", err)
	}
	st.mark("clone")

	// node is the privileged container a nested engine's container on the
	// host's network mode shares the network of, read before this engine's
	// lock, as the engine that runs it may hold its lock while it waits on
	// this one's; the container stays in a stack of its own here.
	var node *Container
	var nodeHost, nodeDomain string
	var nodeAddresses []string
	if req.HostConfig.NetworkMode == "host" {
		_, node = e.hostNode()
		node.mu.Lock()
		nodeHost, nodeDomain, nodeAddresses = hostnameOf(node), node.rec.Config.Domainname, firstAddresses(node.rec.Address, node.rec.Networks)
		node.mu.Unlock()
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	st.mark("lock")
	for _, other := range e.containers {
		if other.rec.Name == name {
			return container.CreateResponse{}, conflict("Conflict. The container name \"/%s\" is already in use by container \"%s\". You have to remove (or rename) that container to be able to reuse that name.", name, other.rec.ID)
		}
	}
	// net is the container whose network this one is on, with network mode
	// container:<id>; it joins that one's stack, and one on a container that
	// is on another's is on that other's.
	var net *Container
	if ref, ok := strings.CutPrefix(string(req.HostConfig.NetworkMode), "container:"); ok {
		o, err := e.containerLocked(ref)
		if err != nil {
			return container.CreateResponse{}, err
		}
		if id, ok := strings.CutPrefix(string(o.rec.HostConfig.NetworkMode), "container:"); ok {
			if o, err = e.containerLocked(id); err != nil {
				return container.CreateResponse{}, err
			}
		}
		net, stack = o, o.rec.Stack
	}
	networks := map[string]*network.EndpointSettings{}
	if net == nil && node == nil {
		var err error
		if networks, err = e.resolveNetworksLocked(stack, name, service, req); err != nil {
			return container.CreateResponse{}, err
		}
	}
	links, err := e.legacyLinksLocked(req.HostConfig)
	if err != nil {
		return container.CreateResponse{}, err
	}

	if _, err := e.alloc.Stack(stack); err != nil {
		return container.CreateResponse{}, invalid("stack %q: %v", stack, err)
	}
	stackDir, err := e.alloc.Dir(stack)
	if err != nil {
		return container.CreateResponse{}, err
	}
	c := &Container{dir: filepath.Join(stackDir, "c", id[:12]), removed: make(chan struct{}), runDone: closedChan(), nextRun: make(chan struct{})}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return container.CreateResponse{}, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(c.dir)
			e.releaseStackIfEmptyLocked(stack)
		}
	}()
	if err := os.Rename(stagedRoot, c.root()); err != nil {
		return container.CreateResponse{}, err
	}

	ports := map[string]int{}
	// On the host's network a container's ports are the node's, as Docker
	// discards the ports a container on the host's network publishes.
	if string(req.HostConfig.NetworkMode) != "none" && net == nil && node == nil {
		for _, p := range exposedPorts(cfg, req.HostConfig) {
			// One host port serves a container port's TCP and UDP alike, so
			// SANDBOX_PORT_<n> names it whatever the protocol.
			hp, err := e.alloc.Port(stack, portKey+"/"+p.Port()+"/tcp")
			if err != nil {
				return container.CreateResponse{}, err
			}
			ports[p.String()] = hp
		}
	}
	routerPort := 0
	if string(req.HostConfig.NetworkMode) != "none" && net == nil && node == nil {
		if routerPort, err = e.alloc.Port(stack, portKey+"/router"); err != nil {
			return container.CreateResponse{}, err
		}
	}

	mounts, err := e.prepareMountsLocked(c.root(), img, cfg, req.HostConfig)
	if err != nil {
		return container.CreateResponse{}, err
	}
	address := ""
	if net == nil && node == nil {
		if address, err = e.assignAddressLocked(stack); err != nil {
			return container.CreateResponse{}, err
		}
	}
	hostname, domain, extraHosts, hostAddress := cfg.Hostname, cfg.Domainname, req.HostConfig.ExtraHosts, firstAddresses(address, networks)
	if req.HostConfig.NetworkMode == "none" {
		hostAddress = nil
	}
	if net != nil {
		// A container on another's network has its hostname and hosts, as
		// it would its UTS namespace's files in Docker.
		hostname, domain, extraHosts, hostAddress = hostnameOf(net), net.rec.Config.Domainname, net.rec.HostConfig.ExtraHosts, firstAddresses(net.rec.Address, net.rec.Networks)
		cfg.Hostname = hostname
	}
	if node != nil {
		// And on the host's network, its hostname and hosts are the node's,
		// unless it names a hostname of its own.
		hostAddress = nodeAddresses
		if hostname == "" {
			hostname, domain = nodeHost, nodeDomain
			cfg.Hostname = hostname
		}
	}
	if hostname == "" {
		hostname = id[:12]
	}
	if err := writeHostsFiles(c.root(), hostname, domain, hostAddress, extraHosts, nil, mounts); err != nil {
		return container.CreateResponse{}, err
	}

	if c.logs, err = openLogStore(filepath.Join(c.dir, "log.jsonl")); err != nil {
		return container.CreateResponse{}, err
	}
	hc := *req.HostConfig
	if hc.NetworkMode == "" || hc.NetworkMode == "default" {
		hc.NetworkMode = "bridge"
	}
	if net != nil {
		hc.NetworkMode = container.NetworkMode("container:" + net.rec.ID)
	}
	if hc.LogConfig.Type == "" {
		hc.LogConfig.Type = "json-file"
	}
	if hc.RestartPolicy.Name == "" {
		hc.RestartPolicy.Name = container.RestartPolicyDisabled
	}
	c.rec = record{
		ID: id, Name: name, Created: time.Now().UTC(),
		Image: req.Config.Image, ImageID: img.ID,
		Config: cfg, HostConfig: hc, Networks: networks,
		Stack: stack, Service: service, Ports: ports, Mounts: mounts,
		Address: address, RouterPort: routerPort, Links: links,
		State: stateRec{Status: container.StateCreated},
	}
	if err := c.save(); err != nil {
		return container.CreateResponse{}, err
	}
	st.mark("prepare")
	if err := os.Rename(stagedInit, c.initDir()); err != nil {
		return container.CreateResponse{}, err
	}
	if err := layInit(c.root(), c.initDir(), initPaths(mounts)); err != nil {
		return container.CreateResponse{}, fmt.Errorf("record the initial root: %w", err)
	}
	st.mark("record")
	e.containers[id] = c
	if err := e.writeNetworkLocked(stack); err != nil {
		delete(e.containers, id)
		return container.CreateResponse{}, err
	}
	ok = true
	e.publishContainer(c, events.ActionCreate, nil)
	return container.CreateResponse{ID: id, Warnings: []string{}}, nil
}

// checkUser accepts a user as Docker does at create: the container's own
// passwd and group files resolve it as it starts.
func (e *Engine) checkUser(u string) error {
	if strings.HasPrefix(u, ":") || strings.Count(u, ":") > 1 {
		return invalid("invalid user %q", u)
	}
	return nil
}

// processEnv is the environment of a process of the container running as
// user: the container's, with the identity and hostname the container
// library presents.
func (e *Engine) processEnv(c *Container, extra []string, user string, tty bool) ([]string, error) {
	id, err := identity.Resolve(c.root(), user)
	if err != nil {
		return nil, err
	}
	if id, err = id.AddGroups(c.root(), c.rec.HostConfig.GroupAdd); err != nil {
		return nil, err
	}
	ne, net, err := e.netOf(c)
	if err != nil {
		return nil, err
	}
	links, err := ne.linksOf(net)
	if err != nil {
		return nil, err
	}
	env, err := e.environment(c, ne, net, extra, tty, linkEnv(links))
	if err != nil {
		return nil, err
	}
	host := hostnameOf(c)
	env = append(env, id.Env()...)
	if v, _, err := e.shmEnv(c); err != nil {
		return nil, err
	} else if v != "" {
		env = append(env, v)
	}
	if c.rec.HostConfig.Privileged {
		env = append(env, EngineVar+"="+e.engineSocket(c))
	}
	// On the host's UTS namespace the library presents no hostname, so the
	// container reads the host's, as it changes.
	if c.rec.HostConfig.UTSMode != "host" {
		env = append(env, pathmap.HostnameVar+"="+host, pathmap.DomainnameVar+"="+c.rec.Config.Domainname)
	}
	pv, err := e.pidnsEnv(c)
	if err != nil {
		return nil, err
	}
	env = append(env, pv...)
	var ms []pathmap.Mount
	for _, m := range c.rec.Mounts {
		ms = append(ms, pathmap.Mount{Dest: m.Destination, Source: m.Source})
	}
	mv, err := pathmap.MountsVar(ms)
	if err != nil {
		return nil, err
	}
	if mv != "" {
		env = append(env, mv)
	}
	ports, err := pathmap.PortsVar(net.rec.Ports)
	if err != nil {
		return nil, err
	}
	if ports != "" {
		env = append(env, ports)
	}
	if helper, err := roles.Binary(); err == nil {
		env = append(env, pathmap.HelperVar+"="+filepath.Base(helper))
	}
	env = append(env, pathmap.PidsVar+"="+c.pidsPath())
	if onNetwork(net) && net.rec.Address != "" {
		nf, err := ne.networkFile(net.rec.Stack)
		if err != nil {
			return nil, err
		}
		env = append(env, pathmap.NetworkVar+"="+nf, pathmap.AddressVar+"="+net.rec.Address)
		if net.rec.RouterPort != 0 {
			env = append(env, pathmap.PortDirVar+"="+ne.portDir(net), pathmap.RouterVar+"="+strconv.Itoa(net.rec.RouterPort))
		}
	}
	return env, nil
}

// checkContainerNetwork refuses what a container on another's network cannot
// have of its own, with Docker's errors.
func checkContainerNetwork(req container.CreateRequest) error {
	hc, cfg := req.HostConfig, req.Config
	if !strings.HasPrefix(string(hc.NetworkMode), "container:") {
		return nil
	}
	switch {
	case cfg.Hostname != "":
		return invalid("conflicting options: hostname and the network mode")
	case len(hc.PortBindings) > 0 || hc.PublishAllPorts:
		return invalid("conflicting options: port publishing and the container type network mode")
	case len(cfg.ExposedPorts) > 0:
		return invalid("conflicting options: port exposing and the container type network mode")
	case len(hc.DNS) > 0 || len(hc.DNSOptions) > 0 || len(hc.DNSSearch) > 0:
		return invalid("conflicting options: dns and the network mode")
	case len(hc.ExtraHosts) > 0:
		return invalid("conflicting options: custom host-to-IP mapping and the network mode")
	case len(hc.Links) > 0:
		return invalid("conflicting options: container type network can't be used with links. This would result in undefined behavior")
	}
	return nil
}

func checkHostConfig(hc *container.HostConfig) error {
	r := hc.Resources
	type knob struct {
		set  bool
		name string
	}
	if _, _, err := containerLimits(r); err != nil {
		return err
	}
	for _, k := range []knob{
		{hc.UsernsMode != "", "user namespaces"},
		{hc.Runtime != "" && hc.Runtime != "runc", "alternative runtimes"},
		{hc.LogConfig.Type != "" && hc.LogConfig.Type != "json-file" && hc.LogConfig.Type != "local", "log drivers other than json-file"},
		{len(hc.Sysctls) > 0, "sysctls"},
	} {
		if k.set {
			return unsupported("%s are not supported: this engine runs without root or a VM and cannot enforce them", k.name)
		}
	}
	for _, k := range []struct {
		set  bool
		what string
	}{
		{len(hc.DNS) > 0 || len(hc.DNSSearch) > 0 || len(hc.DNSOptions) > 0,
			"DNS servers, search domains and options are not supported: a container resolves names through the host's resolver and reaches the internet through its stack's egress proxy, never a DNS server of its own"},
		{hc.OomScoreAdj != 0, "OOM score adjustments are not supported: macOS ends processes under memory pressure by its own measure"},
		{len(hc.StorageOpt) > 0, "storage options are not supported: a container's root is a directory, which no size quota holds"},
		{hc.Cgroup != "", "cgroup sharing is not supported: macOS has no cgroups"},
	} {
		if k.set {
			return unsupported("%s", k.what)
		}
	}
	if hc.Isolation != "" && hc.Isolation != container.IsolationDefault {
		return invalid("invalid isolation: %q - this engine only supports 'default'", hc.Isolation)
	}
	switch hc.CgroupnsMode {
	case "", container.CgroupnsModePrivate, container.CgroupnsModeHost:
	default:
		return invalid("invalid cgroup namespace mode: %s", hc.CgroupnsMode)
	}
	if err := checkSecurity(hc); err != nil {
		return err
	}
	switch pid := string(hc.PidMode); {
	case pid == "", pid == "private", pid == "host", strings.HasPrefix(pid, "container:") && len(pid) > len("container:"):
	default:
		return invalid("invalid PID mode: %q", pid)
	}
	if hc.UTSMode != "" && hc.UTSMode != "host" {
		return invalid("invalid UTS mode: %q", hc.UTSMode)
	}
	switch ipc := string(hc.IpcMode); {
	case ipc == "", ipc == "private", ipc == "shareable", ipc == "host", ipc == "none", strings.HasPrefix(ipc, "container:"):
	default:
		return invalid("invalid IPC mode: %q", ipc)
	}
	for p, bs := range hc.PortBindings {
		if p.Proto() != network.TCP && p.Proto() != network.UDP {
			return unsupported("publishing %s is not supported; only TCP and UDP ports can be published", p)
		}
		for _, b := range bs {
			if _, _, err := portproxy.HostPortRange(b.HostPort); err != nil {
				return invalid("invalid host port %q for %s: %v", b.HostPort, p, err)
			}
		}
	}
	return nil
}

func generateName(id string) string { return "fws_" + id[:12] }

// mergeConfig layers the create request over the image config, as Docker does.
func mergeConfig(img image.Image, req *container.Config) container.Config {
	ic := img.Config.Config
	cfg := *req
	env := map[string]int{}
	var merged []string
	for _, kv := range append(append([]string{}, ic.Env...), req.Env...) {
		k, _, _ := strings.Cut(kv, "=")
		if i, ok := env[k]; ok {
			merged[i] = kv
			continue
		}
		env[k] = len(merged)
		merged = append(merged, kv)
	}
	cfg.Env = merged
	if len(cfg.Entrypoint) == 0 {
		cfg.Entrypoint = ic.Entrypoint
		if len(cfg.Cmd) == 0 {
			cfg.Cmd = ic.Cmd
		}
	}
	if cfg.WorkingDir == "" {
		cfg.WorkingDir = ic.WorkingDir
	}
	if cfg.User == "" {
		cfg.User = ic.User
	}
	if cfg.StopSignal == "" {
		cfg.StopSignal = ic.StopSignal
	}
	if cfg.Healthcheck == nil && ic.Healthcheck != nil {
		h := ic.Healthcheck
		cfg.Healthcheck = &container.HealthConfig{Test: h.Test, Interval: h.Interval, Timeout: h.Timeout, StartPeriod: h.StartPeriod, StartInterval: img.HealthStartInterval, Retries: h.Retries}
	}
	labels := map[string]string{}
	for k, v := range ic.Labels {
		labels[k] = v
	}
	for k, v := range req.Labels {
		labels[k] = v
	}
	cfg.Labels = labels
	ports := network.PortSet{}
	for p := range ic.ExposedPorts {
		if pp, err := network.ParsePort(p); err == nil {
			ports[pp] = struct{}{}
		}
	}
	for p := range req.ExposedPorts {
		ports[p] = struct{}{}
	}
	cfg.ExposedPorts = ports
	vols := map[string]struct{}{}
	for v := range ic.Volumes {
		vols[v] = struct{}{}
	}
	for v := range req.Volumes {
		vols[v] = struct{}{}
	}
	cfg.Volumes = vols
	return cfg
}

func exposedPorts(cfg container.Config, hc *container.HostConfig) []network.Port {
	set := map[network.Port]struct{}{}
	for p := range cfg.ExposedPorts {
		set[p] = struct{}{}
	}
	for p := range hc.PortBindings {
		set[p] = struct{}{}
	}
	out := make([]network.Port, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func (e *Engine) resolveNetworksLocked(stack, name, service string, req container.CreateRequest) (map[string]*network.EndpointSettings, error) {
	out := map[string]*network.EndpointSettings{}
	add := func(netName string, es *network.EndpointSettings) error {
		if netName == "default" {
			netName = "bridge"
		}
		n, err := e.checkNetworkLocked(netName)
		if err != nil {
			return err
		}
		s := &network.EndpointSettings{}
		if es != nil {
			s = es.Copy()
		}
		s.NetworkID = n.ID
		s.EndpointID = newID()
		names := []string{name}
		for _, a := range s.Aliases {
			if a != name {
				names = append(names, a)
			}
		}
		if service != name && n.Name != "bridge" {
			names = append(names, service)
		}
		if n.Name != "bridge" && n.Name != "none" {
			s.DNSNames = uniqStrings(names)
		}
		if err := e.assignNetAddressLocked(n, "", s); err != nil {
			return err
		}
		out[n.Name] = s
		return nil
	}
	if req.NetworkingConfig != nil {
		for netName, es := range req.NetworkingConfig.EndpointsConfig {
			if err := add(netName, es); err != nil {
				return nil, err
			}
		}
	}
	mode := string(req.HostConfig.NetworkMode)
	if mode == "" || mode == "default" {
		mode = "bridge"
	}
	if _, ok := out[mode]; !ok {
		if err := add(mode, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// prepareMountsLocked realises every mount as a symlink at its destination
// inside the container root pointing at the host directory that backs it.
// Seatbelt evaluates the resolved path, so the profile grants the backing
// directory itself.
func (e *Engine) prepareMountsLocked(root string, img image.Image, cfg container.Config, hc *container.HostConfig) ([]mountRec, error) {
	var mounts []mountRec
	taken := map[string]bool{}
	add := func(m mountRec) error {
		dest := path.Clean("/" + m.Destination)
		if dest == "/" {
			return invalid("invalid mount config: destination can't be '/'")
		}
		if taken[dest] {
			return invalid("Duplicate mount point: %s", dest)
		}
		taken[dest] = true
		m.Destination = dest
		mounts = append(mounts, m)
		return nil
	}
	for _, b := range hc.Binds {
		parts := strings.Split(b, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, invalid("invalid volume specification: '%s'", b)
		}
		rw := true
		if len(parts) == 3 {
			for _, o := range strings.Split(parts[2], ",") {
				switch o {
				case "ro":
					rw = false
				case "rw", "z", "Z", "cached", "delegated", "consistent":
				default:
					return nil, invalid("invalid mode: %s", o)
				}
			}
		}
		m := mountRec{Source: parts[0], Destination: parts[1], RW: rw}
		if strings.HasPrefix(parts[0], "/") {
			m.Type = mount.TypeBind
		} else {
			m.Type, m.Name = mount.TypeVolume, parts[0]
		}
		if err := add(m); err != nil {
			return nil, err
		}
	}
	for _, mm := range hc.Mounts {
		m := mountRec{Type: mm.Type, Destination: mm.Target, RW: !mm.ReadOnly}
		switch mm.Type {
		case mount.TypeBind:
			m.Source = mm.Source
		case mount.TypeVolume:
			m.Name = mm.Source
			if o := mm.VolumeOptions; o != nil {
				m.Subpath, m.NoCopy, m.volumeLabels = o.Subpath, o.NoCopy, o.Labels
				if d := o.DriverConfig; d != nil {
					if d.Name != "" && d.Name != "local" {
						return nil, unsupported("volume driver %q is not supported; only \"local\" is", d.Name)
					}
					if err := checkVolumeOptions(d.Options); err != nil {
						return nil, err
					}
					m.volumeOpts = d.Options
				}
			}
		case mount.TypeImage:
			img, err := e.images.Get(mm.Source)
			if err != nil {
				return nil, notFound("No such image: %s", mm.Source)
			}
			m.Name, m.RW = img.ID, false
			if o := mm.ImageOptions; o != nil {
				m.Subpath = o.Subpath
			}
		case mount.TypeTmpfs:
		default:
			return nil, unsupported("mount type %q is not supported", mm.Type)
		}
		if err := add(m); err != nil {
			return nil, err
		}
	}
	for dest := range hc.Tmpfs {
		if err := add(mountRec{Type: mount.TypeTmpfs, Destination: dest, RW: true}); err != nil {
			return nil, err
		}
	}
	// --volumes-from mounts another container's volumes and binds where it
	// mounts them, read-only with :ro, read-write with :rw, and else as it
	// does; a mount of the container's own at the same place wins, as in
	// Docker. Its tmpfs mounts are its own.
	for _, spec := range hc.VolumesFrom {
		ref, mode, _ := strings.Cut(spec, ":")
		var force *bool
		for _, o := range strings.Split(mode, ",") {
			switch o {
			case "":
			case "ro", "rw":
				rw := o == "rw"
				force = &rw
			case "z", "Z":
			default:
				return nil, invalid("invalid mode for volumes-from: %s", mode)
			}
		}
		src, err := e.containerLocked(ref)
		if err != nil {
			return nil, err
		}
		for _, m := range src.rec.Mounts {
			if m.Type == mount.TypeTmpfs || taken[m.Destination] {
				continue
			}
			if force != nil {
				m.RW = *force
			}
			if err := add(m); err != nil {
				return nil, err
			}
		}
	}
	vols := make([]string, 0, len(cfg.Volumes))
	for v := range cfg.Volumes {
		vols = append(vols, v)
	}
	sort.Strings(vols)
	for _, v := range vols {
		if !taken[path.Clean("/"+v)] {
			if err := add(mountRec{Type: mount.TypeVolume, Destination: v, RW: true}); err != nil {
				return nil, err
			}
		}
	}

	for i := range mounts {
		m := &mounts[i]
		switch m.Type {
		case mount.TypeBind:
			src, err := e.hostPath(m.Source)
			if err != nil {
				return nil, err
			}
			if _, err := os.Stat(src); err != nil {
				if !os.IsNotExist(err) {
					return nil, err
				}
				// Binds through the Binds list create a missing source, as
				// Docker does; the Mounts API refuses one.
				if !containsBind(hc.Binds, m.Source) {
					return nil, invalid("invalid mount config for type \"bind\": bind source path does not exist: %s", m.Source)
				}
				if err := os.MkdirAll(src, 0o755); err != nil {
					// A read-only bind of a path the host cannot hold, as
					// kind binds /lib/modules, which macOS's sealed root
					// refuses, mounts an empty directory: what the
					// container reads there, Linux's kernel modules, the
					// host has none of.
					if m.RW || !(errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)) {
						return nil, err
					}
					src = filepath.Join(filepath.Dir(root), "absent", strings.ReplaceAll(strings.TrimPrefix(m.Destination, "/"), "/", "_"))
					if err := os.MkdirAll(src, 0o555); err != nil {
						return nil, err
					}
				}
			}
			if src, err = filepath.EvalSymlinks(src); err != nil {
				return nil, err
			}
			m.Source = src
		case mount.TypeVolume:
			anonymous := m.Name == ""
			if anonymous {
				m.Name = newID()
			}
			v, ok := e.volumes[m.Name]
			if !ok {
				var err error
				if v, err = e.createVolumeLocked(m.Name, m.volumeLabels, m.volumeOpts, anonymous); err != nil {
					return nil, err
				}
			}
			src, err := e.volumeSource(v)
			if err != nil {
				return nil, err
			}
			if v.Fresh && m.Subpath == "" && !m.NoCopy && emptyDir(src) {
				// A new, empty volume starts with the image's content at the
				// mount point, as Docker's copy-up does.
				from, err := rootfs.Join(e.images.Root(img.ID), m.Destination)
				if err != nil {
					return nil, err
				}
				if err := copyTree(from, src); err != nil {
					return nil, err
				}
				v.Fresh = false
				if err := e.saveVolumesLocked(); err != nil {
					return nil, err
				}
			}
			if m.Source, err = subpath(src, m.Subpath); err != nil {
				return nil, err
			}
		case mount.TypeImage:
			var err error
			if m.Source, err = subpath(e.images.Root(m.Name), m.Subpath); err != nil {
				return nil, err
			}
		case mount.TypeTmpfs:
			m.Source = filepath.Join(filepath.Dir(root), "tmpfs", strings.ReplaceAll(strings.TrimPrefix(m.Destination, "/"), "/", "_"))
			if err := os.MkdirAll(m.Source, 0o1777); err != nil {
				return nil, err
			}
		}
		if err := placeMount(root, m.Destination, m.Source); err != nil {
			return nil, err
		}
	}
	return mounts, nil
}

// placeMount links source at destination in root. It resolves the
// destination's directory as the container sees it, so no link in the
// image leads the writes out of the root.
func placeMount(root, destination, source string) error {
	dir, err := rootfs.Join(root, path.Dir(destination))
	if err != nil {
		return err
	}
	dest := filepath.Join(dir, path.Base(destination))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	// The link names the source by its real path, which a program in the
	// container that follows it walks on the host.
	real, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	return os.Symlink(real, dest)
}

// placeInit links the engine's helper binary, which runs as the init by
// that name, at the init's path in root, as Docker mounts docker-init.
func placeInit(root string) error {
	self, err := roles.Binary()
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}
	dest, err := rootfs.Join(root, runtime.InitPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return os.Symlink(real, dest)
}

// subpath is the file or directory sub names inside dir, resolved as inside a
// root, so a symlink cannot lead out of it; it must exist, as Docker
// requires.
func subpath(dir, sub string) (string, error) {
	if sub == "" {
		return dir, nil
	}
	p, err := rootfs.Join(dir, sub)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err != nil {
		return "", invalid("cannot access path %s: %v", sub, err)
	}
	return filepath.EvalSymlinks(p)
}

func emptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

func containsBind(binds []string, src string) bool {
	for _, b := range binds {
		if strings.HasPrefix(b, src+":") {
			return true
		}
	}
	return false
}

func copyTree(src, dst string) error {
	fi, err := os.Lstat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return nil
	}
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(target, fi.Mode().Perm()|0o700)
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		case fi.Mode().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
		return nil
	})
}

// envName turns a service name into the form used in SANDBOX_<NAME>_PORT_<n>.
func envName(s string) string {
	b := []byte(strings.ToUpper(s))
	for i, c := range b {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '_'
		}
	}
	return string(b)
}

// environment is the process environment: the image and container variables
// with PATH mapped into the root, plus the variables through which the
// service learns its root, its own ports and its peers' ports; net is the
// container whose network it is on.
// environment is c's environment on net's network, which engine ne runs:
// its egress proxy and ports are net's stack's.
func (e *Engine) environment(c *Container, ne *Engine, net *Container, extra []string, tty bool, links []string) ([]string, error) {
	root := c.root()
	vars := map[string]string{}
	var order []string
	set := func(kv string) {
		k, v, _ := strings.Cut(kv, "=")
		if _, ok := vars[k]; !ok {
			order = append(order, k)
		}
		vars[k] = v
	}
	set("PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	set("HOME=" + filepath.Join(root, "root"))
	set("HOSTNAME=" + hostnameOf(c))
	if tty {
		set("TERM=xterm")
	}
	for _, kv := range links {
		set(kv)
	}
	for _, kv := range c.rec.Config.Env {
		set(kv)
	}
	for _, kv := range extra {
		set(kv)
	}
	var mapped []string
	for _, dir := range filepath.SplitList(vars["PATH"]) {
		if strings.HasPrefix(dir, "/") {
			mapped = append(mapped, filepath.Join(root, dir))
		}
	}
	vars["PATH"] = strings.Join(mapped, ":")
	if h := vars["HOME"]; !strings.HasPrefix(h, root) {
		vars["HOME"] = filepath.Join(root, h)
	}
	home, err := rootfs.Join(root, strings.TrimPrefix(vars["HOME"], root))
	if err != nil {
		return nil, err
	}
	os.MkdirAll(home, 0o755)
	set("SANDBOX_ROOT=" + root)
	set("SANDBOX_STACK=" + c.rec.Stack)
	set("SANDBOX_HOST=127.0.0.1")
	// POSIX shared memory and semaphore names must start with the prefix
	// under private IPC, so programs that name them need it.
	if _, prefix, err := e.ipcMode(c); err == nil && prefix != "" {
		set("SANDBOX_IPC_PREFIX=" + prefix)
	}
	if port := ne.egressPort(net.rec.Stack); port != 0 && ne.hasEgress(net) {
		member, password, err := ne.egressCredentials(net.rec.Stack, net.rec.Address)
		if err != nil {
			return nil, err
		}
		for _, kv := range egress.Variables(port, member, password) {
			if _, ok := vars[kv[0]]; !ok {
				set(kv[0] + "=" + kv[1])
			}
		}
	}
	for _, p := range sortedKeys(net.rec.Ports) {
		num, _, _ := strings.Cut(p, "/")
		set(fmt.Sprintf("SANDBOX_PORT_%s=%d", num, net.rec.Ports[p]))
	}
	for _, peer := range e.peers(c) {
		for _, p := range sortedKeys(peer.ports) {
			num, _, _ := strings.Cut(p, "/")
			for _, n := range uniqStrings([]string{peer.service, peer.name}) {
				set(fmt.Sprintf("SANDBOX_%s_HOST=127.0.0.1", envName(n)))
				set(fmt.Sprintf("SANDBOX_%s_PORT_%s=%d", envName(n), num, peer.ports[p]))
			}
		}
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+vars[k])
	}
	return out, nil
}

// peer is what a container's environment needs of another in its stack.
type peer struct {
	service, name, number string
	ports                 map[string]int
}

// peers lists the stack's other containers. environment lets later entries
// win, so each service's replica 1 comes last and SANDBOX_<SERVICE>_* name it.
// It copies what it needs while holding e.mu, under which rename changes a
// name; services, ports and labels never change after create.
func (e *Engine) peers(c *Container) []peer {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []peer
	for _, o := range e.containers {
		if o != c && o.rec.Stack == c.rec.Stack {
			out = append(out, peer{service: o.rec.Service, name: o.rec.Name, number: o.rec.Config.Labels[LabelContainerNumber], ports: o.rec.Ports})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].service != out[j].service {
			return out[i].service < out[j].service
		}
		return out[i].number > out[j].number
	})
	return out
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// profile is the Seatbelt spec for any process of the container.
func (e *Engine) profile(c *Container) (seatbelt.Spec, error) {
	s, err := e.alloc.Stack(c.rec.Stack)
	if err != nil {
		return seatbelt.Spec{}, err
	}
	ipc, prefix, err := e.ipcMode(c)
	if err != nil {
		return seatbelt.Spec{}, err
	}
	lib, err := e.Library()
	if err != nil {
		return seatbelt.Spec{}, err
	}
	spec := seatbelt.Spec{
		Root: c.root(), StateRoot: e.stateRoot(), ReadOnlyRoot: c.rec.HostConfig.ReadonlyRootfs,
		IPC: ipc, IPCPrefix: prefix, Libraries: []string{lib}, ReadFiles: []string{c.pidsPath()},
	}
	if c.rec.HostConfig.Privileged {
		spec.Sockets = append(spec.Sockets, e.engineSocket(c))
	}
	if _, ledger, err := e.shmEnv(c); err != nil {
		return seatbelt.Spec{}, err
	} else if ledger != "" {
		spec.ReadFiles = append(spec.ReadFiles, ledger)
		spec.WriteFiles = append(spec.WriteFiles, ledger)
	}
	owner, err := e.pidOwner(c)
	if err != nil {
		return seatbelt.Spec{}, err
	}
	if spec.HostPIDs = owner.rec.HostConfig.PidMode.IsHost(); !spec.HostPIDs {
		spec.ReadFiles = append(spec.ReadFiles, e.pidnsFile(owner))
		spec.ProcsSocket = e.procsSocket(c)
	}
	for _, m := range c.rec.Mounts {
		spec.Volumes = append(spec.Volumes, seatbelt.Volume{Path: m.Source, ReadOnly: !m.RW})
	}
	// A container on another's network binds that one's ports and listens
	// in its port directory, as processes of one network namespace do; a
	// nested engine's container on the host's network mode, the
	// privileged container's, in that one's stack.
	ne, net, err := e.netOf(c)
	if err != nil {
		return seatbelt.Spec{}, err
	}
	if net.rec.HostConfig.NetworkMode == "none" {
		return spec, nil
	}
	if ne != e {
		if s, err = ne.alloc.Stack(net.rec.Stack); err != nil {
			return seatbelt.Spec{}, err
		}
	}
	nf, err := ne.networkFile(net.rec.Stack)
	if err != nil {
		return seatbelt.Spec{}, err
	}
	spec.ReadFiles = append(spec.ReadFiles, nf)
	if net.rec.RouterPort != 0 {
		spec.PortDir = ne.portDir(net)
	}
	for _, p := range sortedKeys(net.rec.Ports) {
		spec.BindPorts = append(spec.BindPorts, net.rec.Ports[p])
	}
	// Every port of the stack's block is reachable except the egress proxy,
	// which only containers with internet access may use.
	proxy := ne.egressPort(net.rec.Stack)
	withEgress := ne.hasEgress(net)
	for p := s.PortBase; p < s.PortBase+s.BlockSize; p++ {
		if p == proxy && !withEgress {
			continue
		}
		spec.ConnectPorts = append(spec.ConnectPorts, p)
	}
	return spec, nil
}

func (c *Container) argv() []string {
	return append(append([]string{}, c.rec.Config.Entrypoint...), c.rec.Config.Cmd...)
}

func (e *Engine) Start(ref string) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	running := c.rec.State.Status == container.StateRunning || c.starting
	c.mu.Unlock()
	if running {
		return notModified("container already started")
	}
	err = c.start(e)
	if err != nil && c.rec.HostConfig.AutoRemove {
		// dockerd removes an auto-remove container whose start fails; a
		// client waiting for its removal would otherwise wait forever.
		e.Remove(c.rec.ID, RemoveOptions{Force: true, RemoveVolumes: true})
	}
	return err
}

func (c *Container) start(e *Engine) error {
	c.mu.Lock()
	if c.restarting != nil {
		c.restarting.Stop()
		c.restarting = nil
	}
	c.starting = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.starting = false
		c.mu.Unlock()
	}()

	st := e.steps("start " + c.rec.ID[:12])
	argv := c.argv()
	if len(argv) == 0 {
		return invalid("no command specified")
	}
	if _, net, err := e.netOf(c); err != nil {
		return err
	} else if net != c && !net.isRunning() {
		return conflict("cannot join network namespace of a non running container: %s", net.rec.ID)
	}
	spec, err := e.profile(c)
	if err != nil {
		return err
	}
	st.mark("profile")
	limits, res, err := containerLimits(c.rec.HostConfig.Resources)
	if err != nil {
		return err
	}
	for _, m := range c.rec.Mounts {
		if m.Type == mount.TypeTmpfs {
			os.RemoveAll(m.Source)
			if err := os.MkdirAll(m.Source, 0o1777); err != nil {
				return err
			}
		}
	}
	if err := e.emptyTmpfsVolumes(c); err != nil {
		return err
	}
	if err := c.writePidsLimit(c.rec.HostConfig.PidsLimit); err != nil {
		return err
	}
	if owner, err := e.shmOwner(c); err != nil {
		return err
	} else if owner == c {
		if err := clearShm(c); err != nil {
			return err
		}
	}
	if err := checkEndpointLinks(c); err != nil {
		return err
	}
	if err := e.writeHosts(c); err != nil {
		return err
	}
	st.mark("files")
	if ne, net, err := e.netOf(c); err != nil {
		return err
	} else if ne.hasEgress(net) {
		if _, err := ne.ensureEgress(net.rec.Stack); err != nil {
			return err
		}
	}
	st.mark("egress")
	plan, err := e.publishPlan(c)
	if err != nil {
		return err
	}
	nsFile, _, err := e.joinPIDNamespace(c)
	if err != nil {
		return err
	}
	env, err := e.processEnv(c, nil, c.rec.Config.User, c.rec.Config.Tty)
	if err != nil {
		if KindOf(err) != KindInternal {
			return err
		}
		return invalid("%v", err)
	}
	if c.rec.HostConfig.Privileged {
		if err := e.openNested(c); err != nil {
			return err
		}
	}
	if hc := c.rec.HostConfig; hc.Init != nil && *hc.Init {
		if err := placeInit(c.root()); err != nil {
			return err
		}
		argv = append([]string{runtime.InitPath, "--"}, argv...)
	}
	cfg := shim.Config{
		ID: c.rec.ID,
		Spec: runtime.Spec{
			Root: c.root(), Argv: argv, Env: env, WorkDir: c.rec.Config.WorkingDir,
			Profile: spec, ProfilePath: filepath.Join(c.dir, "profile.sb"), Limits: limits, Library: spec.Libraries[0],
		},
		TTY: c.rec.Config.Tty, Console: c.rec.HostConfig.ConsoleSize, OpenStdin: c.rec.Config.OpenStdin,
		LogPath: c.logs.path, Socket: e.shimSocket(c), Anchor: e.anchorSocket(c), ExitPath: c.exitPath(),
		Router: c.rec.RouterPort, PortDir: e.portDir(c),
		Tag: c.tag(), AllocRoot: e.cfg.Root, Stack: c.rec.Stack, Resources: res,
	}
	if nsFile != "" {
		cfg.Procs, cfg.PidNamespace = e.procsSocket(c), nsFile
	}
	if c.rec.RouterPort != 0 {
		if cfg.NetworkFile, err = e.networkFile(c.rec.Stack); err != nil {
			return err
		}
		cfg.Address = c.rec.Address
	}
	for _, p := range plan {
		cfg.Publish = append(cfg.Publish, p.binding)
	}
	st.mark("prepare")
	task, ready, err := shim.Launch(c.dir, cfg)
	st.mark("launch")
	if err != nil {
		if errors.Is(err, shim.ErrPortInUse) {
			return conflict("%v", err)
		}
		c.mu.Lock()
		c.rec.State.Error = err.Error()
		c.rec.State.ExitCode = 127
		c.save()
		c.mu.Unlock()
		return invalid("failed to create task for container: %v", err)
	}
	published := network.PortMap{}
	for i, p := range plan {
		port, ip := ready.Ports[i], p.binding.IP
		if p.report != 0 {
			port, ip = p.report, netip.IPv4Unspecified()
		}
		published[p.port] = append(published[p.port], network.PortBinding{HostIP: ip, HostPort: strconv.Itoa(port)})
	}

	c.mu.Lock()
	c.rec.State = stateRec{
		Status: container.StateRunning, StartedAt: time.Now().UTC(), Pid: task.Pid(),
		RestartCount: c.rec.State.RestartCount,
	}
	c.rec.Published = published
	done := c.attachTaskLocked(e, task, false)
	c.save()
	c.mu.Unlock()
	e.publishContainer(c, events.ActionStart, nil)
	go e.watch(c, task, done)
	st.mark("record")
	st.log("shim: " + ready.Steps)
	return nil
}

// attachTaskLocked makes task the container's current run and starts what
// follows a run: its health checks, carried on from the record when the
// engine reconnects to the run.
func (c *Container) attachTaskLocked(e *Engine, task *shim.Client, reconnect bool) chan struct{} {
	done := make(chan struct{})
	c.task = task
	c.runDone = done
	close(c.nextRun)
	c.nextRun = make(chan struct{})
	if hc := c.rec.Config.Healthcheck; hc != nil && len(hc.Test) > 0 && hc.Test[0] != "NONE" {
		if !reconnect || c.rec.State.Health == nil {
			c.rec.State.Health = &container.Health{Status: container.Starting}
		}
		c.healthStop = make(chan struct{})
		go e.monitorHealth(c, *hc, c.healthStop, done)
	} else {
		c.rec.State.Health = nil
	}
	return done
}

func (c *Container) exitPath() string { return filepath.Join(c.dir, "exit.json") }

// pidsPath is the file of the container's pids limit, which its processes
// read before they fork, as a pids cgroup's pids.max holds it.
func (c *Container) pidsPath() string { return filepath.Join(c.dir, "pids.max") }

// writePidsLimit writes the container's pids limit to its file.
func (c *Container) writePidsLimit(limit *int64) error {
	v := "max"
	if limit != nil && *limit > 0 {
		v = strconv.FormatInt(*limit, 10)
	}
	tmp := c.pidsPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(v+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.pidsPath())
}

// shimSocket is where the container's shim listens: under the engine's
// root, kept short, as macOS caps socket paths at 104 bytes.
func (e *Engine) shimSocket(c *Container) string {
	return filepath.Join(e.runDir(), c.rec.ID[:12]+".sock")
}

// portDir holds the Unix sockets the container library listens on for the
// container's TCP ports that have no host port: outside the root, where
// only the container and its shim reach them.
func (e *Engine) portDir(c *Container) string {
	return filepath.Join(e.runDir(), c.rec.ID[:12]+".p")
}

func (e *Engine) anchorSocket(c *Container) string {
	return filepath.Join(e.runDir(), c.rec.ID[:12]+".anchor")
}

// anchor is the socket of the running container's exec anchor, which
// starts execs in its sandbox, or "" for a run an engine without anchors
// started, whose execs enter the profile by themselves.
func (e *Engine) anchor(c *Container) string {
	p := e.anchorSocket(c)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// watch follows a run until its shim reports the main process's end, then
// records it and applies the restart policy. It returns early when the
// engine detaches from its containers, leaving them running.
func (e *Engine) watch(c *Container, task *shim.Client, done chan struct{}) {
	stopLogs := make(chan struct{})
	go task.Logs(stopLogs, c.logs.wake)
	exit, err := task.Wait(e.detach)
	close(stopLogs)
	select {
	case <-e.detach:
		return
	default:
	}
	if err != nil {
		exit = e.lostExit(c)
	}
	c.logs.wake()
	relayShimLog(c)
	e.ended(c, exit, done)
}

// relayShimLog copies what the run's shim logged about itself into the
// engine's log, where a supervisor's failure is seen, and empties it.
func relayShimLog(c *Container) {
	path := filepath.Join(c.dir, shim.LogFile)
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		log.Printf("container %s supervisor: %s", c.rec.ID[:12], line)
	}
	os.Truncate(path, 0)
}

// lostExit is how a run ended whose shim went away without answering: as
// the shim recorded before it exited, or, when the shim itself died, ended
// by the engine, which kills what is left of the container.
func (e *Engine) lostExit(c *Container) shim.Exit {
	if exit, err := shim.ReadExit(c.exitPath()); err == nil {
		return exit
	}
	if err := procs.KillTagged(c.tag()); err != nil {
		log.Printf("container %s: %v", c.rec.ID[:12], err)
	}
	return shim.Exit{ExitCode: 255, FinishedAt: time.Now().UTC()}
}

func (e *Engine) ended(c *Container, exit shim.Exit, done chan struct{}) {
	code := exit.ExitCode
	c.mu.Lock()
	c.rec.Published = nil
	if c.healthStop != nil {
		close(c.healthStop)
		c.healthStop = nil
	}
	c.rec.State.Status = container.StateExited
	c.rec.State.Paused = false
	c.rec.State.ExitCode = code
	c.rec.State.OOMKilled = exit.OOMKilled
	c.rec.State.FinishedAt = exit.FinishedAt
	c.rec.State.Pid = 0
	manual := c.rec.State.ManualStop
	policy := c.rec.HostConfig.RestartPolicy
	if err := e.leavePIDNamespace(c); err != nil {
		log.Printf("container %s: %v", c.rec.ID[:12], err)
	}
	restarts := c.rec.State.RestartCount
	runFor := c.rec.State.FinishedAt.Sub(c.rec.State.StartedAt)
	c.save()
	c.task = nil
	c.mu.Unlock()
	if c.rec.HostConfig.Privileged {
		e.closeNested(c)
	}
	close(done)
	if exit.OOMKilled {
		e.publishContainer(c, events.ActionOOM, nil)
	}
	e.publishContainer(c, events.ActionDie, map[string]string{"exitCode": strconv.Itoa(code)})

	restart := !manual && (policy.Name == container.RestartPolicyAlways || policy.Name == container.RestartPolicyUnlessStopped ||
		(policy.Name == container.RestartPolicyOnFailure && code != 0 && (policy.MaximumRetryCount == 0 || restarts < policy.MaximumRetryCount)))
	if restart {
		// Docker doubles the delay from 100ms on each restart and resets it
		// once a run lasts ten seconds.
		delay := 100 * time.Millisecond
		if runFor < 10*time.Second {
			for i := 0; i < restarts && delay < time.Minute; i++ {
				delay *= 2
			}
		}
		c.mu.Lock()
		c.rec.State.Status = container.StateRestarting
		c.rec.State.RestartCount++
		c.save()
		c.restarting = time.AfterFunc(delay, func() {
			if err := c.start(e); err != nil {
				c.logs.append("stderr", fmt.Sprintf("restart failed: %v\n", err))
				c.mu.Lock()
				c.rec.State.Status = container.StateExited
				c.save()
				c.mu.Unlock()
			}
		})
		c.mu.Unlock()
		return
	}
	if c.rec.HostConfig.AutoRemove {
		e.Remove(c.rec.ID, RemoveOptions{RemoveVolumes: true})
	}
}

func parseSignal(s string) (syscall.Signal, error) {
	if s == "" {
		return syscall.SIGTERM, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return syscall.Signal(n), nil
	}
	name := strings.TrimPrefix(strings.ToUpper(s), "SIG")
	sigs := map[string]syscall.Signal{
		"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "KILL": syscall.SIGKILL,
		"USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "TERM": syscall.SIGTERM, "STOP": syscall.SIGSTOP,
		"CONT": syscall.SIGCONT, "WINCH": syscall.SIGWINCH, "ALRM": syscall.SIGALRM, "PIPE": syscall.SIGPIPE,
	}
	if sig, ok := sigs[name]; ok {
		return sig, nil
	}
	return 0, invalid("Invalid signal: %s", s)
}

// Stop signals the main process and kills the container once the timeout
// passes. timeout nil means the container's StopTimeout or 10s; negative
// means wait forever.
func (e *Engine) Stop(ref string, timeout *int, signal string) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.restarting != nil {
		c.restarting.Stop()
		c.restarting = nil
		c.rec.State.Status = container.StateExited
		c.rec.State.ManualStop = true
		c.save()
		c.mu.Unlock()
		return nil
	}
	running := c.rec.State.Status == container.StateRunning
	c.mu.Unlock()
	if !running {
		return notModified("container already stopped")
	}
	if signal != "" {
		if _, err := parseSignal(signal); err != nil {
			return err
		}
	}
	return c.stopWith(e, timeout, signal)
}

func (c *Container) stopWith(e *Engine, timeout *int, signal string) error {
	c.mu.Lock()
	task, done := c.task, c.runDone
	if task == nil {
		c.mu.Unlock()
		return nil
	}
	c.rec.State.ManualStop = true
	if signal == "" {
		signal = c.rec.Config.StopSignal
	}
	wait := defaultStopTimeout
	if c.rec.Config.StopTimeout != nil {
		wait = time.Duration(*c.rec.Config.StopTimeout) * time.Second
	}
	if timeout != nil {
		wait = time.Duration(*timeout) * time.Second
	}
	c.save()
	c.mu.Unlock()

	sig, err := parseSignal(signal)
	if err != nil {
		sig = syscall.SIGTERM
	}
	e.publishContainer(c, events.ActionKill, map[string]string{"signal": strconv.Itoa(int(sig))})
	task.Signal(sig)
	// A stopped process acts on its pending signal once it continues.
	if c.isPaused() {
		c.mu.Lock()
		c.rec.State.Paused = false
		c.save()
		c.mu.Unlock()
		task.Unpause()
	}
	if wait >= 0 {
		select {
		case <-done:
		case <-time.After(wait):
			e.publishContainer(c, events.ActionKill, map[string]string{"signal": "9"})
			// The shim ends every process of the container once the main
			// one is gone.
			task.Signal(syscall.SIGKILL)
			<-done
		}
	} else {
		<-done
	}
	e.publishContainer(c, events.ActionStop, nil)
	return nil
}

func (e *Engine) Kill(ref, signal string) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	if signal == "" {
		signal = "KILL"
	}
	sig, err := parseSignal(signal)
	if err != nil {
		return err
	}
	c.mu.Lock()
	task := c.task
	if task == nil || c.rec.State.Status != container.StateRunning {
		c.mu.Unlock()
		return conflict("Cannot kill container: %s: container %s is not running", ref, c.rec.ID)
	}
	if sig == syscall.SIGKILL || sig == syscall.SIGTERM || sig == syscall.SIGINT {
		c.rec.State.ManualStop = true
		c.save()
	}
	c.mu.Unlock()
	e.publishContainer(c, events.ActionKill, map[string]string{"signal": strconv.Itoa(int(sig))})
	return task.Signal(sig)
}

func (e *Engine) Restart(ref string, timeout *int, signal string) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	if c.isRunning() {
		if err := c.stopWith(e, timeout, signal); err != nil {
			return err
		}
	}
	if err := c.start(e); err != nil {
		return err
	}
	e.publishContainer(c, events.ActionRestart, nil)
	return nil
}

func (e *Engine) Rename(ref, name string) error {
	name = strings.TrimPrefix(name, "/")
	if !validName.MatchString(name) {
		return invalid("Invalid container name (%s), only [a-zA-Z0-9][a-zA-Z0-9_.-] are allowed", name)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, err := e.containerLocked(ref)
	if err != nil {
		return err
	}
	for _, o := range e.containers {
		if o != c && o.rec.Name == name {
			return conflict("Conflict. The container name \"/%s\" is already in use by container \"%s\".", name, o.rec.ID)
		}
	}
	c.mu.Lock()
	old := c.rec.Name
	c.nameMu.Lock()
	c.rec.Name = name
	c.nameMu.Unlock()
	for _, es := range c.rec.Networks {
		for i, n := range es.DNSNames {
			if n == old {
				es.DNSNames[i] = name
			}
		}
	}
	err = c.save()
	c.mu.Unlock()
	if err == nil {
		err = e.writeNetworkLocked(c.rec.Stack)
	}
	e.publishContainer(c, events.ActionRename, map[string]string{"oldName": "/" + old})
	return err
}

type RemoveOptions struct {
	Force         bool
	RemoveVolumes bool
}

func (e *Engine) Remove(ref string, opts RemoveOptions) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.restarting != nil {
		c.restarting.Stop()
		c.restarting = nil
	}
	task, done := c.task, c.runDone
	running := c.rec.State.Status == container.StateRunning
	c.rec.State.ManualStop = true
	c.mu.Unlock()
	if running {
		if !opts.Force {
			return conflict("cannot remove container \"/%s\": container is running: stop the container before removing or force remove", c.Name())
		}
		e.publishContainer(c, events.ActionKill, map[string]string{"signal": "9"})
		if task != nil {
			task.Signal(syscall.SIGKILL)
		}
		<-done
	}
	if c.rec.HostConfig.Privileged {
		if err := e.removeNested(c); err != nil {
			return err
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.containers[c.rec.ID]; !ok {
		return nil
	}
	delete(e.containers, c.rec.ID)
	if err := e.writeNetworkLocked(c.rec.Stack); err != nil {
		return err
	}
	for id, x := range e.execs {
		if x.containerID == c.rec.ID {
			delete(e.execs, id)
		}
	}
	c.logs.close()
	if err := e.discard(c.dir); err != nil {
		return err
	}
	if opts.RemoveVolumes {
		for _, m := range c.rec.Mounts {
			if m.Type == mount.TypeVolume {
				if v, ok := e.volumes[m.Name]; ok && v.Anonymous && !e.volumeInUseLocked(m.Name) {
					e.removeVolumeLocked(m.Name)
				}
			}
		}
	}
	e.releaseStackIfEmptyLocked(c.rec.Stack)
	close(c.removed)
	e.publishContainer(c, events.ActionDestroy, nil)
	return nil
}

func (e *Engine) releaseStackIfEmptyLocked(stack string) {
	for _, c := range e.containers {
		if c.rec.Stack == stack {
			return
		}
	}
	e.closeEgress(stack)
	e.alloc.Release(stack)
}

// Wait blocks until the condition holds and returns the exit code.
func (e *Engine) Wait(ref string, condition container.WaitCondition) (<-chan container.WaitResponse, error) {
	c, err := e.Container(ref)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	done, removed, next := c.runDone, c.removed, c.nextRun
	running := c.rec.State.Status == container.StateRunning || c.rec.State.Status == container.StateRestarting || c.starting
	c.mu.Unlock()
	out := make(chan container.WaitResponse, 1)
	go func() {
		switch condition {
		case container.WaitConditionRemoved:
			<-removed
		case container.WaitConditionNextExit:
			if !running {
				select {
				case <-next:
					c.mu.Lock()
					done = c.runDone
					c.mu.Unlock()
				case <-removed:
					out <- container.WaitResponse{StatusCode: int64(c.exitCode())}
					return
				}
			}
			<-done
		default:
			if running {
				<-done
			}
		}
		out <- container.WaitResponse{StatusCode: int64(c.exitCode())}
	}()
	return out, nil
}

func (c *Container) exitCode() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rec.State.ExitCode
}

func (e *Engine) publishContainer(c *Container, action events.Action, extra map[string]string) {
	attrs := map[string]string{"name": c.Name(), "image": c.rec.Image}
	for k, v := range c.rec.Config.Labels {
		attrs[k] = v
	}
	for k, v := range extra {
		attrs[k] = v
	}
	e.events.publish(events.ContainerEventType, action, c.rec.ID, attrs)
}

// Prune removes every stopped container the filters select.
func (e *Engine) Prune(f Filters) ([]string, uint64, error) {
	var removed []string
	var reclaimed uint64
	for _, c := range e.allContainers() {
		c.mu.Lock()
		stopped := c.rec.State.Status == container.StateExited || c.rec.State.Status == container.StateCreated || c.rec.State.Status == container.StateDead
		labels := c.rec.Config.Labels
		created := c.rec.Created
		c.mu.Unlock()
		if !stopped || !f.matchLabels(labels) {
			continue
		}
		before, err := f.until(created)
		if err != nil {
			return removed, reclaimed, err
		}
		if !before {
			continue
		}
		size := dirSize(c.dir)
		if err := e.Remove(c.rec.ID, RemoveOptions{}); err != nil {
			return removed, reclaimed, err
		}
		removed = append(removed, c.rec.ID)
		reclaimed += uint64(size)
	}
	return removed, reclaimed, nil
}
