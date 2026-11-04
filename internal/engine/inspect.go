package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/storage"
)

func (e *Engine) Inspect(ref string) (container.InspectResponse, error) {
	c, err := e.Container(ref)
	if err != nil {
		return container.InspectResponse{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rec
	argv := append(append([]string{}, r.Config.Entrypoint...), r.Config.Cmd...)
	var path string
	var args []string
	if len(argv) > 0 {
		path, args = argv[0], argv[1:]
	}
	if args == nil {
		args = []string{}
	}
	st := &container.State{
		Status: c.stateName(), Running: r.State.Status == container.StateRunning, Paused: c.rec.State.Paused,
		Restarting: r.State.Status == container.StateRestarting, Dead: r.State.Status == container.StateDead,
		Pid: r.State.Pid, ExitCode: r.State.ExitCode, Error: r.State.Error, OOMKilled: r.State.OOMKilled,
		StartedAt: timeString(r.State.StartedAt), FinishedAt: timeString(r.State.FinishedAt),
	}
	if c.rec.State.Health != nil {
		h := *c.rec.State.Health
		st.Health = &h
	}
	cfg := r.Config
	cfg.Image = r.Image
	if cfg.Hostname == "" {
		cfg.Hostname = r.ID[:12]
	}
	hc := r.HostConfig
	var execIDs []string
	for id := range e.execsFor(r.ID) {
		execIDs = append(execIDs, id)
	}
	return container.InspectResponse{
		ID: r.ID, Created: timeString(r.Created), Path: path, Args: args, State: st,
		Image: r.ImageID, Name: "/" + r.Name, RestartCount: r.State.RestartCount,
		Driver: "apfs-clone", Platform: "darwin", ExecIDs: execIDs,
		LogPath:    filepath.Join(c.dir, "log.jsonl"),
		HostConfig: &hc, Config: &cfg, Mounts: c.mountPoints(),
		GraphDriver: &storage.DriverData{Name: "apfs-clone", Data: map[string]string{"MergedDir": c.root()}},
		NetworkSettings: &container.NetworkSettings{
			SandboxID: r.ID, Ports: c.portMap(), Networks: copyEndpoints(r.Networks),
		},
	}, nil
}

func (e *Engine) execsFor(id string) map[string]*Exec {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]*Exec{}
	for xid, x := range e.execs {
		if x.containerID == id {
			out[xid] = x
		}
	}
	return out
}

func copyEndpoints(in map[string]*network.EndpointSettings) map[string]*network.EndpointSettings {
	out := map[string]*network.EndpointSettings{}
	for k, v := range in {
		out[k] = v.Copy()
	}
	return out
}

func (c *Container) mountPoints() []container.MountPoint {
	out := []container.MountPoint{}
	for _, m := range c.rec.Mounts {
		mp := container.MountPoint{Type: m.Type, Name: m.Name, Source: m.Source, Destination: m.Destination, RW: m.RW}
		if m.Type == "volume" {
			mp.Driver = "local"
		}
		mode := "rw"
		if !m.RW {
			mode = "ro"
		}
		mp.Mode = mode
		out = append(out, mp)
	}
	return out
}

// portMap reports exposed ports with their published host bindings, the
// shape `docker port` and Compose read.
func (c *Container) portMap() network.PortMap {
	out := network.PortMap{}
	for p := range c.rec.Config.ExposedPorts {
		out[p] = nil
	}
	for p, bs := range c.rec.Published {
		out[p] = bs
	}
	return out
}

func (c *Container) status() string {
	s := c.rec.State
	switch s.Status {
	case container.StateRunning:
		h := ""
		if c.rec.State.Health != nil {
			switch c.rec.State.Health.Status {
			case container.Starting:
				h = " (health: starting)"
			default:
				h = " (" + string(c.rec.State.Health.Status) + ")"
			}
		}
		if c.rec.State.Paused {
			h = " (Paused)"
		}
		return "Up " + humanDuration(time.Since(s.StartedAt)) + h
	case container.StateExited:
		if s.FinishedAt.IsZero() {
			return "Exited (" + strconv.Itoa(s.ExitCode) + ")"
		}
		return fmt.Sprintf("Exited (%d) %s ago", s.ExitCode, humanDuration(time.Since(s.FinishedAt)))
	case container.StateRestarting:
		return fmt.Sprintf("Restarting (%d) %s ago", s.ExitCode, humanDuration(time.Since(s.FinishedAt)))
	case container.StateCreated:
		return "Created"
	}
	return string(s.Status)
}

func humanDuration(d time.Duration) string {
	switch s := int(d.Seconds()); {
	case s < 1:
		return "Less than a second"
	case s == 1:
		return "1 second"
	case s < 60:
		return fmt.Sprintf("%d seconds", s)
	case s < 120:
		return "About a minute"
	case s < 3600:
		return fmt.Sprintf("%d minutes", s/60)
	case s < 7200:
		return "About an hour"
	case s < 48*3600:
		return fmt.Sprintf("%d hours", s/3600)
	default:
		return fmt.Sprintf("%d days", s/86400)
	}
}

type ListOptions struct {
	All     bool
	Limit   int
	Filters Filters
	// Size reports each container's writes since create and its root's
	// whole size, as docker ps --size does.
	Size bool
}

func (e *Engine) List(opts ListOptions) []container.Summary {
	var out []container.Summary
	for _, c := range e.allContainers() {
		c.mu.Lock()
		s, ok := c.summary(opts)
		c.mu.Unlock()
		if !ok {
			continue
		}
		if opts.Size {
			if rw, err := c.sizeRw(context.Background()); err == nil {
				s.SizeRw = rw
				if img, err := e.images.Get(c.rec.ImageID); err == nil {
					s.SizeRootFs = img.Size + rw
				}
			}
		}
		out = append(out, s)
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	if out == nil {
		out = []container.Summary{}
	}
	return out
}

func (c *Container) summary(opts ListOptions) (container.Summary, bool) {
	r := c.rec
	f := opts.Filters
	running := r.State.Status == container.StateRunning
	if !opts.All && !running && !f.has("status") {
		return container.Summary{}, false
	}
	health := string(container.NoHealthcheck)
	if c.rec.State.Health != nil {
		health = string(c.rec.State.Health.Status)
	}
	var nets []string
	for n := range r.Networks {
		nets = append(nets, n)
	}
	if !f.matchName("name", r.Name) || !f.matchLabels(r.Config.Labels) || !f.matchExact("status", string(c.stateName())) ||
		!f.matchExact("health", health) || !f.matchExact("network", nets...) || !matchID(f, r.ID) ||
		!f.matchExact("ancestor", r.Image, r.ImageID) {
		return container.Summary{}, false
	}
	if f.has("exited") && (r.State.Status != container.StateExited || !f.matchExact("exited", strconv.Itoa(r.State.ExitCode))) {
		return container.Summary{}, false
	}
	if f.has("volume") {
		var vols []string
		for _, m := range r.Mounts {
			vols = append(vols, m.Name, m.Destination)
		}
		if !f.matchExact("volume", vols...) {
			return container.Summary{}, false
		}
	}
	var ports []container.PortSummary
	for p := range r.Config.ExposedPorts {
		bs := r.Published[p]
		if len(bs) == 0 {
			ports = append(ports, container.PortSummary{PrivatePort: p.Num(), Type: string(p.Proto())})
		}
		for _, b := range bs {
			hp, _ := strconv.Atoi(b.HostPort)
			ports = append(ports, container.PortSummary{IP: b.HostIP, PrivatePort: p.Num(), PublicPort: uint16(hp), Type: string(p.Proto())})
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].PrivatePort < ports[j].PrivatePort })
	if ports == nil {
		ports = []container.PortSummary{}
	}
	argv := append(append([]string{}, r.Config.Entrypoint...), r.Config.Cmd...)
	s := container.Summary{
		ID: r.ID, Names: []string{"/" + r.Name}, Image: r.Image, ImageID: r.ImageID,
		Command: strings.Join(argv, " "), Created: r.Created.Unix(), Ports: ports,
		Labels: r.Config.Labels, State: c.stateName(), Status: c.status(),
		NetworkSettings: &container.NetworkSettingsSummary{Networks: copyEndpoints(r.Networks)},
		Mounts:          c.mountPoints(),
	}
	s.HostConfig.NetworkMode = string(r.HostConfig.NetworkMode)
	if c.rec.State.Health != nil {
		s.Health = &container.HealthSummary{Status: c.rec.State.Health.Status, FailingStreak: c.rec.State.Health.FailingStreak}
	}
	return s, true
}

func matchID(f Filters, id string) bool {
	if !f.has("id") {
		return true
	}
	for _, want := range f["id"] {
		if strings.HasPrefix(id, want) {
			return true
		}
	}
	return false
}

type LogOptions struct {
	Stdout, Stderr bool
	Follow         bool
	Since, Until   time.Time
	Tail           int // negative means all
}

// Logs streams the container's stored output, then new output while
// following, until the container stops or ctx ends.
func (e *Engine) Logs(ctx context.Context, ref string, opts LogOptions, fn func(LogEntry) error) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	done := c.runDone
	tty := c.rec.Config.Tty
	c.mu.Unlock()
	want := func(le LogEntry) bool {
		if !tty && (le.Stream == "stdout" && !opts.Stdout || le.Stream == "stderr" && !opts.Stderr) {
			return false
		}
		if !opts.Since.IsZero() && le.Time.Before(opts.Since) {
			return false
		}
		return opts.Until.IsZero() || !le.Time.After(opts.Until)
	}
	return c.logs.followEntries(ctx, followOptions{follow: opts.Follow, done: done, tail: opts.Tail, want: want}, fn)
}
