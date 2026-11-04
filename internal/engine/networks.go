package engine

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
)

// Network records a Docker network. Containers reach each other through their
// stack's ports, so a network carries no traffic of its own; it scopes which
// containers may join it and is reported faithfully to clients.
type Network struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Created    time.Time         `json:"created"`
	Labels     map[string]string `json:"labels"`
	Options    map[string]string `json:"options"`
	Internal   bool              `json:"internal"`
	Attachable bool              `json:"attachable"`
	Predefined bool              `json:"predefined"`
}

func (e *Engine) networksPath() string { return filepath.Join(e.cfg.Root, "networks.json") }

func (e *Engine) loadNetworks() error {
	if _, err := readJSON(e.networksPath(), &e.networks); err != nil {
		return err
	}
	if e.networks == nil {
		e.networks = map[string]*Network{}
	}
	for _, n := range []struct{ name, driver string }{{"bridge", "bridge"}, {"host", "host"}, {"none", "null"}} {
		if _, err := e.networkLocked(n.name); err != nil {
			id := newID()
			e.networks[id] = &Network{ID: id, Name: n.name, Driver: n.driver, Created: time.Now().UTC(), Predefined: true, Labels: map[string]string{}, Options: map[string]string{}}
		}
	}
	return e.saveNetworksLocked()
}

func (e *Engine) saveNetworksLocked() error { return writeJSON(e.networksPath(), e.networks) }

func (e *Engine) networkLocked(ref string) (*Network, error) {
	if n, ok := e.networks[ref]; ok {
		return n, nil
	}
	for _, n := range e.networks {
		if n.Name == ref {
			return n, nil
		}
	}
	var match *Network
	for id, n := range e.networks {
		if len(ref) > 0 && strings.HasPrefix(id, ref) {
			if match != nil {
				return nil, invalid("network %s is ambiguous", ref)
			}
			match = n
		}
	}
	if match == nil {
		return nil, notFound("network %s not found", ref)
	}
	return match, nil
}

type NetworkCreate struct {
	Name       string
	Driver     string
	Internal   bool
	Attachable bool
	EnableIPv6 *bool
	IPAM       *network.IPAM
	Options    map[string]string
	Labels     map[string]string
}

func (e *Engine) NetworkCreate(req NetworkCreate) (network.CreateResponse, error) {
	if req.Name == "" {
		return network.CreateResponse{}, invalid("network name is required")
	}
	if req.Driver == "" {
		req.Driver = "bridge"
	}
	if req.Driver != "bridge" {
		return network.CreateResponse{}, unsupported("network driver %q is not supported; only \"bridge\" is", req.Driver)
	}
	if req.IPAM != nil && len(req.IPAM.Config) > 0 {
		return network.CreateResponse{}, unsupported("network %s: custom IPAM subnets are not supported; containers reach each other through their stack's ports", req.Name)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.networkLocked(req.Name); err == nil {
		return network.CreateResponse{}, conflict("network with name %s already exists", req.Name)
	}
	labels := req.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	opts := req.Options
	if opts == nil {
		opts = map[string]string{}
	}
	n := &Network{ID: newID(), Name: req.Name, Driver: req.Driver, Created: time.Now().UTC(), Labels: labels, Options: opts, Internal: req.Internal, Attachable: req.Attachable}
	e.networks[n.ID] = n
	if err := e.saveNetworksLocked(); err != nil {
		delete(e.networks, n.ID)
		return network.CreateResponse{}, err
	}
	e.events.publish(events.NetworkEventType, events.ActionCreate, n.ID, map[string]string{"name": n.Name, "type": n.Driver})
	return network.CreateResponse{ID: n.ID}, nil
}

func (e *Engine) NetworkInspect(ref string) (network.Inspect, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, err := e.networkLocked(ref)
	if err != nil {
		return network.Inspect{}, err
	}
	return e.networkView(n), nil
}

func (e *Engine) networkView(n *Network) network.Inspect {
	containers := map[string]network.EndpointResource{}
	for _, c := range e.containers {
		if _, ok := c.rec.Networks[n.Name]; ok && c.isRunning() {
			containers[c.rec.ID] = network.EndpointResource{Name: c.rec.Name, EndpointID: c.rec.Networks[n.Name].EndpointID}
		}
	}
	return network.Inspect{
		Network: network.Network{
			Name: n.Name, ID: n.ID, Created: n.Created, Scope: "local", Driver: n.Driver,
			EnableIPv4: n.Driver == "bridge", IPAM: network.IPAM{Driver: "default", Config: []network.IPAMConfig{}},
			Internal: n.Internal, Attachable: n.Attachable, Options: n.Options, Labels: n.Labels,
		},
		Containers: containers,
	}
}

func (e *Engine) NetworkList(f Filters) []network.Summary {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []network.Summary
	for _, n := range e.networks {
		if !f.matchName("name", n.Name) || !f.matchExact("id", n.ID) || !f.matchExact("driver", n.Driver) || !f.matchLabels(n.Labels) {
			continue
		}
		if f.has("type") {
			typ := "custom"
			if n.Predefined {
				typ = "builtin"
			}
			if !f.matchExact("type", typ) {
				continue
			}
		}
		out = append(out, network.Summary{Network: e.networkView(n).Network})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (e *Engine) NetworkRemove(ref string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, err := e.networkLocked(ref)
	if err != nil {
		return err
	}
	if n.Predefined {
		return errorf(KindConflict, "%s is a pre-defined network and cannot be removed", n.Name)
	}
	for _, c := range e.containers {
		if _, ok := c.rec.Networks[n.Name]; ok {
			return conflict("error while removing network: network %s has active endpoints (%s)", n.Name, c.rec.Name)
		}
	}
	delete(e.networks, n.ID)
	e.events.publish(events.NetworkEventType, events.ActionDestroy, n.ID, map[string]string{"name": n.Name, "type": n.Driver})
	return e.saveNetworksLocked()
}

// checkNetworkLocked refuses networks a container's stack may not join: one
// stack's containers never share a network with another stack's.
func (e *Engine) checkNetworkLocked(name, stack string) (*Network, error) {
	n, err := e.networkLocked(name)
	if err != nil {
		return nil, err
	}
	switch n.Name {
	case "host":
		return nil, unsupported("network mode \"host\" is not supported: every container is confined to its own stack's ports")
	case "bridge", "none":
		return n, nil
	}
	if owner, ok := n.Labels[LabelProject]; ok && owner != stack {
		return nil, unsupported("network %s belongs to stack %q; a container of stack %q cannot join it", n.Name, owner, stack)
	}
	return n, nil
}

// NetworkConnect adds the container to the network. Membership scopes names
// within the stack; it opens no path to another stack.
func (e *Engine) NetworkConnect(ref string, req network.ConnectRequest) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, err := e.networkLocked(ref)
	if err != nil {
		return err
	}
	c, err := e.containerLocked(req.Container)
	if err != nil {
		return err
	}
	if strings.HasPrefix(string(c.rec.HostConfig.NetworkMode), "container:") {
		return forbidden("container sharing network namespace with another container or host cannot be connected to any other network")
	}
	if _, err := e.checkNetworkLocked(n.Name, c.rec.Stack); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.rec.Networks[n.Name]; ok {
		return conflict("endpoint with name %s already exists in network %s", c.rec.Name, n.Name)
	}
	s := &network.EndpointSettings{}
	if req.EndpointConfig != nil {
		s = req.EndpointConfig.Copy()
	}
	s.NetworkID, s.EndpointID = n.ID, newID()
	s.DNSNames = uniqStrings(append(append([]string{c.rec.Name}, s.Aliases...), c.rec.Service))
	c.rec.Networks[n.Name] = s
	e.events.publish(events.NetworkEventType, events.ActionConnect, n.ID, map[string]string{"container": c.rec.ID, "name": n.Name, "type": n.Driver})
	if err := c.save(); err != nil {
		return err
	}
	return e.writeNetworkLocked(c.rec.Stack)
}

func (e *Engine) NetworkDisconnect(ref, containerRef string, force bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, err := e.networkLocked(ref)
	if err != nil {
		return err
	}
	c, err := e.containerLocked(containerRef)
	if err != nil {
		if force {
			return nil
		}
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.rec.Networks[n.Name]; !ok {
		return invalid("container %s is not connected to network %s", c.rec.Name, n.Name)
	}
	delete(c.rec.Networks, n.Name)
	e.events.publish(events.NetworkEventType, events.ActionDisconnect, n.ID, map[string]string{"container": c.rec.ID, "name": n.Name, "type": n.Driver})
	if err := c.save(); err != nil {
		return err
	}
	return e.writeNetworkLocked(c.rec.Stack)
}

// NetworkPrune removes custom networks no container uses.
func (e *Engine) NetworkPrune(f Filters) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var removed []string
	for id, n := range e.networks {
		if n.Predefined || !f.matchLabels(n.Labels) {
			continue
		}
		used := false
		for _, c := range e.containers {
			if _, ok := c.rec.Networks[n.Name]; ok {
				used = true
			}
		}
		if used {
			continue
		}
		delete(e.networks, id)
		removed = append(removed, n.Name)
		e.events.publish(events.NetworkEventType, events.ActionDestroy, n.ID, map[string]string{"name": n.Name, "type": n.Driver})
	}
	return removed, e.saveNetworksLocked()
}
