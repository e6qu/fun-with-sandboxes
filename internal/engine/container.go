package engine

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
)

const (
	LabelContainerNumber = "com.docker.compose.container-number"
	defaultStopTimeout   = 10 * time.Second
	// outputDrain bounds how long the engine reads output after a process
	// group is gone, for descendants that escaped the group.
	outputDrain = 2 * time.Second
)

type mountRec struct {
	Type        mount.Type `json:"type"`
	Name        string     `json:"name,omitempty"`
	Source      string     `json:"source"`
	Destination string     `json:"destination"`
	RW          bool       `json:"rw"`
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
	Mounts    []mountRec      `json:"mounts"`
	State     stateRec        `json:"state"`
}

type Container struct {
	mu   sync.Mutex
	rec  record
	dir  string
	logs *logStore

	proc    *runtime.Process
	runDone chan struct{}
	// nextRun closes when the next run starts.
	nextRun    chan struct{}
	removed    chan struct{}
	health     *container.Health
	healthStop chan struct{}
	proxies    []io.Closer
	stdin      io.WriteCloser
	ptyMaster  *os.File
	restarting *time.Timer
	starting   bool
}

func (c *Container) ID() string   { return c.rec.ID }
func (c *Container) Name() string { return c.rec.Name }
func (c *Container) root() string { return filepath.Join(c.dir, "r") }

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
		if c.rec.State.Status == container.StateRunning || c.rec.State.Status == container.StateRestarting {
			// The previous engine died with the container running; its
			// processes went with it.
			c.rec.State.Status = container.StateExited
			c.rec.State.ExitCode = 255
			c.rec.State.FinishedAt = time.Now().UTC()
			c.rec.State.Pid = 0
			if p := c.rec.HostConfig.RestartPolicy.Name; (p == container.RestartPolicyAlways || p == container.RestartPolicyUnlessStopped) && !c.rec.State.ManualStop {
				restart = append(restart, c)
			}
			if err := c.save(); err != nil {
				return err
			}
		}
		e.containers[c.rec.ID] = c
	}
	for _, c := range restart {
		if err := c.start(e); err != nil {
			c.logs.append("stderr", fmt.Sprintf("restart after engine start failed: %v\n", err))
		}
	}
	return nil
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

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, other := range e.containers {
		if other.rec.Name == name {
			return container.CreateResponse{}, conflict("Conflict. The container name \"/%s\" is already in use by container \"%s\". You have to remove (or rename) that container to be able to reuse that name.", name, other.rec.ID)
		}
	}
	networks, err := e.resolveNetworksLocked(stack, name, service, req)
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
	if err := e.images.Clone(img.ID, c.root()); err != nil {
		return container.CreateResponse{}, fmt.Errorf("clone image root: %w", err)
	}

	ports := map[string]int{}
	if string(req.HostConfig.NetworkMode) != "none" {
		for _, p := range exposedPorts(cfg, req.HostConfig) {
			hp, err := e.alloc.Port(stack, portKey+"/"+p.String())
			if err != nil {
				return container.CreateResponse{}, err
			}
			ports[p.String()] = hp
		}
	}

	mounts, err := e.prepareMountsLocked(c.root(), img, cfg, req.HostConfig)
	if err != nil {
		return container.CreateResponse{}, err
	}

	if c.logs, err = openLogStore(filepath.Join(c.dir, "log.jsonl")); err != nil {
		return container.CreateResponse{}, err
	}
	hc := *req.HostConfig
	if hc.NetworkMode == "" || hc.NetworkMode == "default" {
		hc.NetworkMode = "bridge"
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
		State: stateRec{Status: container.StateCreated},
	}
	if err := c.save(); err != nil {
		return container.CreateResponse{}, err
	}
	e.containers[id] = c
	ok = true
	e.publishContainer(c, events.ActionCreate, nil)
	return container.CreateResponse{ID: id, Warnings: []string{}}, nil
}

func (e *Engine) checkUser(u string) error {
	if u == "" || u == e.user.Uid || u == e.user.Username {
		return nil
	}
	return unsupported("user %q: containers run as the engine's own user (%s); switching users needs root", u, e.user.Username)
}

func checkHostConfig(hc *container.HostConfig) error {
	r := hc.Resources
	type knob struct {
		set  bool
		name string
	}
	for _, k := range []knob{
		{r.Memory != 0 || r.MemorySwap != 0 || r.MemoryReservation != 0, "memory limits"},
		{r.NanoCPUs != 0 || r.CPUQuota != 0 || r.CPUPeriod != 0 || r.CPUShares != 0 || r.CpusetCpus != "" || r.CPUCount != 0 || r.CPUPercent != 0, "CPU limits"},
		{r.PidsLimit != nil && *r.PidsLimit > 0, "PID limits"},
		{len(r.Ulimits) > 0, "ulimits"},
		{len(r.Devices) > 0 || len(r.DeviceRequests) > 0 || len(r.DeviceCgroupRules) > 0, "devices"},
		{r.BlkioWeight != 0 || len(r.BlkioWeightDevice) > 0 || len(r.BlkioDeviceReadBps) > 0 || len(r.BlkioDeviceWriteBps) > 0, "block I/O limits"},
		{hc.Privileged, "privileged mode"},
		{len(hc.CapAdd) > 0, "added capabilities"},
		{len(hc.VolumesFrom) > 0, "volumes-from"},
		{hc.PidMode != "" && hc.PidMode != "private", "PID modes"},
		{hc.IpcMode != "" && hc.IpcMode != "private" && hc.IpcMode != "shareable", "IPC modes"},
		{hc.UTSMode != "", "UTS modes"},
		{hc.UsernsMode != "", "user namespaces"},
		{hc.ReadonlyRootfs, "read-only root filesystems"},
		{hc.Runtime != "" && hc.Runtime != "runc", "alternative runtimes"},
		{hc.LogConfig.Type != "" && hc.LogConfig.Type != "json-file" && hc.LogConfig.Type != "local", "log drivers other than json-file"},
		{len(hc.Sysctls) > 0, "sysctls"},
	} {
		if k.set {
			return unsupported("%s are not supported: this engine runs without root or a VM and cannot enforce them", k.name)
		}
	}
	mode := string(hc.NetworkMode)
	switch {
	case mode == "host":
		return unsupported("network mode \"host\" is not supported: every container is confined to its own stack's ports")
	case strings.HasPrefix(mode, "container:"):
		return unsupported("network mode %q is not supported: containers do not share network stacks", mode)
	}
	for p, bs := range hc.PortBindings {
		if p.Proto() != network.TCP {
			return unsupported("publishing %s is not supported; only TCP ports can be published", p)
		}
		for _, b := range bs {
			if b.HostPort != "" && strings.Contains(b.HostPort, "-") {
				return unsupported("host port ranges (%s) are not supported", b.HostPort)
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
		cfg.Healthcheck = &container.HealthConfig{Test: h.Test, Interval: h.Interval, Timeout: h.Timeout, StartPeriod: h.StartPeriod, Retries: h.Retries}
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
		n, err := e.checkNetworkLocked(netName, stack)
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
			if mm.VolumeOptions != nil && mm.VolumeOptions.Subpath != "" {
				return nil, unsupported("volume subpaths are not supported")
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
		dest := filepath.Join(root, m.Destination)
		switch m.Type {
		case mount.TypeBind:
			src, err := filepath.Abs(m.Source)
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
					return nil, err
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
				if v, err = e.createVolumeLocked(m.Name, nil, anonymous); err != nil {
					return nil, err
				}
			}
			m.Source = e.VolumeDir(m.Name)
			if v.Fresh {
				// A new volume starts with the image's content at the mount
				// point, as Docker's copy-up does.
				if err := copyTree(filepath.Join(e.images.Root(img.ID), m.Destination), m.Source); err != nil {
					return nil, err
				}
				v.Fresh = false
				if err := e.saveVolumesLocked(); err != nil {
					return nil, err
				}
			}
		case mount.TypeTmpfs:
			m.Source = filepath.Join(filepath.Dir(root), "tmpfs", strings.ReplaceAll(strings.TrimPrefix(m.Destination, "/"), "/", "_"))
			if err := os.MkdirAll(m.Source, 0o1777); err != nil {
				return nil, err
			}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, err
		}
		if err := os.RemoveAll(dest); err != nil {
			return nil, err
		}
		if err := os.Symlink(m.Source, dest); err != nil {
			return nil, err
		}
	}
	return mounts, nil
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
// service learns its root, its own ports and its peers' ports.
func (e *Engine) environment(c *Container, extra []string) []string {
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
	os.MkdirAll(vars["HOME"], 0o755)
	set("HOSTNAME=" + c.rec.ID[:12])
	set("SANDBOX_ROOT=" + root)
	set("SANDBOX_STACK=" + c.rec.Stack)
	set("SANDBOX_HOST=127.0.0.1")
	for _, p := range sortedKeys(c.rec.Ports) {
		num, _, _ := strings.Cut(p, "/")
		set(fmt.Sprintf("SANDBOX_PORT_%s=%d", num, c.rec.Ports[p]))
	}
	for _, peer := range e.peers(c) {
		for _, p := range sortedKeys(peer.rec.Ports) {
			num, _, _ := strings.Cut(p, "/")
			for _, n := range uniqStrings([]string{peer.rec.Service, peer.rec.Name}) {
				set(fmt.Sprintf("SANDBOX_%s_HOST=127.0.0.1", envName(n)))
				set(fmt.Sprintf("SANDBOX_%s_PORT_%s=%d", envName(n), num, peer.rec.Ports[p]))
			}
		}
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+vars[k])
	}
	return out
}

// peers lists the stack's other containers. environment lets later entries
// win, so each service's replica 1 comes last and SANDBOX_<SERVICE>_* name it.
func (e *Engine) peers(c *Container) []*Container {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []*Container
	for _, o := range e.containers {
		if o != c && o.rec.Stack == c.rec.Stack {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].rec, out[j].rec
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Labels()[LabelContainerNumber] > b.Labels()[LabelContainerNumber]
	})
	return out
}

func (r record) Labels() map[string]string { return r.Config.Labels }

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
	spec := seatbelt.Spec{Root: c.root(), StateRoot: e.cfg.Root}
	for _, m := range c.rec.Mounts {
		spec.Volumes = append(spec.Volumes, seatbelt.Volume{Path: m.Source, ReadOnly: !m.RW})
	}
	if c.rec.HostConfig.NetworkMode == "none" {
		return spec, nil
	}
	for _, p := range sortedKeys(c.rec.Ports) {
		spec.BindPorts = append(spec.BindPorts, c.rec.Ports[p])
	}
	for p := s.PortBase; p < s.PortBase+s.BlockSize; p++ {
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

	argv := c.argv()
	if len(argv) == 0 {
		return invalid("no command specified")
	}
	spec, err := e.profile(c)
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
	proxies, published, err := e.publish(c)
	if err != nil {
		return err
	}
	closeAll := func() {
		for _, p := range proxies {
			p.Close()
		}
	}

	rs := runtime.Spec{
		Root: c.root(), Argv: argv, Env: e.environment(c, nil), WorkDir: c.rec.Config.WorkingDir,
		Profile: spec, ProfilePath: filepath.Join(c.dir, "profile.sb"),
	}
	stdout := &lineWriter{stream: "stdout", store: c.logs}
	stderr := &lineWriter{stream: "stderr", store: c.logs}
	var stdin io.WriteCloser
	var master, slave *os.File
	if c.rec.Config.Tty {
		master, slave, err = pty.Open()
		if err != nil {
			closeAll()
			return err
		}
		if s := c.rec.HostConfig.ConsoleSize; s[0] > 0 && s[1] > 0 {
			pty.Setsize(master, &pty.Winsize{Rows: uint16(s[0]), Cols: uint16(s[1])})
		}
		rs.TTY = slave
		stdin = master
	} else {
		rs.Stdout, rs.Stderr = stdout, stderr
		if c.rec.Config.OpenStdin {
			pr, pw := io.Pipe()
			rs.Stdin = pr
			stdin = pw
		}
	}
	proc, err := runtime.Start(rs)
	if slave != nil {
		slave.Close()
	}
	if err != nil {
		closeAll()
		if master != nil {
			master.Close()
		}
		c.mu.Lock()
		c.rec.State.Error = err.Error()
		c.rec.State.ExitCode = 127
		c.save()
		c.mu.Unlock()
		return invalid("failed to create task for container: %v", err)
	}
	if master != nil {
		go func() {
			io.Copy(stdout, master)
		}()
	}

	done := make(chan struct{})
	c.mu.Lock()
	c.proc = proc
	c.proxies = proxies
	c.stdin = stdin
	c.ptyMaster = master
	c.runDone = done
	close(c.nextRun)
	c.nextRun = make(chan struct{})
	c.rec.State = stateRec{
		Status: container.StateRunning, StartedAt: time.Now().UTC(), Pid: proc.Pid(),
		RestartCount: c.rec.State.RestartCount,
	}
	c.rec.Published = published
	c.save()
	if hc := c.rec.Config.Healthcheck; hc != nil && len(hc.Test) > 0 && hc.Test[0] != "NONE" {
		c.health = &container.Health{Status: container.Starting}
		c.healthStop = make(chan struct{})
		go e.monitorHealth(c, *hc, c.healthStop, done)
	} else {
		c.health = nil
	}
	c.mu.Unlock()
	e.publishContainer(c, events.ActionStart, nil)
	go e.onExit(c, proc, done, stdout, stderr)
	return nil
}

func (e *Engine) onExit(c *Container, proc *runtime.Process, done chan struct{}, stdout, stderr *lineWriter) {
	<-proc.Done()
	// A container ends with its main process: whatever it left running in
	// its process group goes with it.
	proc.KillAll()
	proc.DrainIO(outputDrain)
	stdout.flush()
	stderr.flush()
	code := proc.ExitCode()

	c.mu.Lock()
	for _, p := range c.proxies {
		p.Close()
	}
	c.proxies = nil
	c.rec.Published = nil
	if c.healthStop != nil {
		close(c.healthStop)
		c.healthStop = nil
	}
	if c.stdin != nil {
		c.stdin.Close()
		c.stdin = nil
	}
	if c.ptyMaster != nil {
		c.ptyMaster.Close()
		c.ptyMaster = nil
	}
	c.rec.State.Status = container.StateExited
	c.rec.State.ExitCode = code
	c.rec.State.FinishedAt = time.Now().UTC()
	c.rec.State.Pid = 0
	manual := c.rec.State.ManualStop
	policy := c.rec.HostConfig.RestartPolicy
	restarts := c.rec.State.RestartCount
	runFor := c.rec.State.FinishedAt.Sub(c.rec.State.StartedAt)
	c.save()
	c.proc = nil
	c.mu.Unlock()
	close(done)
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
	proc, done := c.proc, c.runDone
	if proc == nil {
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
	proc.Signal(sig)
	if wait >= 0 {
		select {
		case <-done:
		case <-time.After(wait):
			e.publishContainer(c, events.ActionKill, map[string]string{"signal": "9"})
			proc.KillAll()
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
	proc := c.proc
	if proc == nil || c.rec.State.Status != container.StateRunning {
		c.mu.Unlock()
		return conflict("Cannot kill container: %s: container %s is not running", ref, c.rec.ID)
	}
	if sig == syscall.SIGKILL || sig == syscall.SIGTERM || sig == syscall.SIGINT {
		c.rec.State.ManualStop = true
		c.save()
	}
	c.mu.Unlock()
	e.publishContainer(c, events.ActionKill, map[string]string{"signal": strconv.Itoa(int(sig))})
	if sig == syscall.SIGKILL {
		proc.KillAll()
		return nil
	}
	return proc.Signal(sig)
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
	c.rec.Name = name
	for _, es := range c.rec.Networks {
		for i, n := range es.DNSNames {
			if n == old {
				es.DNSNames[i] = name
			}
		}
	}
	err = c.save()
	c.mu.Unlock()
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
	proc, done := c.proc, c.runDone
	running := c.rec.State.Status == container.StateRunning
	c.rec.State.ManualStop = true
	c.mu.Unlock()
	if running {
		if !opts.Force {
			return conflict("cannot remove container \"/%s\": container is running: stop the container before removing or force remove", c.rec.Name)
		}
		e.publishContainer(c, events.ActionKill, map[string]string{"signal": "9"})
		proc.KillAll()
		<-done
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.containers[c.rec.ID]; !ok {
		return nil
	}
	delete(e.containers, c.rec.ID)
	for id, x := range e.execs {
		if x.containerID == c.rec.ID {
			delete(e.execs, id)
		}
	}
	c.logs.close()
	if err := os.RemoveAll(c.dir); err != nil {
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
	attrs := map[string]string{"name": c.rec.Name, "image": c.rec.Image}
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
		c.mu.Unlock()
		if !stopped || !f.matchLabels(labels) {
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
