package engine

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// linkRec is a legacy link on the default bridge network: the container
// reaches Child, by ID, as Alias. Docker names the link /<parent>/<alias>.
type linkRec struct {
	Child string `json:"child"`
	Alias string `json:"alias"`
}

// parseLink reads a link as Docker does: name, name:alias, or as inspect
// reports one, /name:/parent/alias.
func parseLink(s string) (string, string, error) {
	if s == "" {
		return "", "", fmt.Errorf("empty string specified for links")
	}
	f := strings.Split(s, ":")
	switch {
	case len(f) > 2:
		return "", "", fmt.Errorf("bad format for links: %s", s)
	case len(f) == 1:
		return s, s, nil
	case strings.HasPrefix(f[0], "/"):
		return f[0][1:], path.Base(f[1]), nil
	}
	return f[0], f[1], nil
}

// userDefinedMode reports whether a network mode names a user-defined
// network, on which Docker leaves legacy links unregistered.
func userDefinedMode(m container.NetworkMode) bool {
	switch s := string(m); {
	case s == "" || s == "default" || s == "bridge" || s == "host" || s == "none":
		return false
	case strings.HasPrefix(s, "container:"):
		return false
	}
	return true
}

// legacyLinksLocked resolves a new container's links to the containers
// they name, as Docker registers them at create: a link to a container on
// another's network links to that other, and a second link of one alias
// is dropped.
func (e *Engine) legacyLinksLocked(hc *container.HostConfig) ([]linkRec, error) {
	if userDefinedMode(hc.NetworkMode) {
		return nil, nil
	}
	var out []linkRec
	seen := map[string]bool{}
	for _, l := range hc.Links {
		name, alias, err := parseLink(l)
		if err != nil {
			return nil, invalid("%v", err)
		}
		child, err := e.containerLocked(name)
		if err != nil {
			return nil, invalid("could not get container for %s: %v", name, err)
		}
		if id, ok := strings.CutPrefix(string(child.rec.HostConfig.NetworkMode), "container:"); ok {
			if child, err = e.containerLocked(id); err != nil {
				return nil, invalid("could not get container for %s: %v", id, err)
			}
		}
		if !seen[alias] {
			seen[alias] = true
			out = append(out, linkRec{Child: child.rec.ID, Alias: alias})
		}
	}
	return out, nil
}

// link is a legacy link of a container to a running child.
type link struct {
	name  string // /<parent>/<alias>
	alias string
	child *Container
}

// linksOf returns c's legacy links, failing as Docker's start does on a
// child that is not running or not on the default bridge network. A
// container off the default bridge network has none in effect.
func (e *Engine) linksOf(c *Container) ([]link, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.linksOfLocked(c)
}

func (e *Engine) linksOfLocked(c *Container) ([]link, error) {
	if c.rec.Networks["bridge"] == nil {
		return nil, nil
	}
	var out []link
	for _, l := range c.rec.Links {
		name := "/" + c.rec.Name + "/" + l.Alias
		child, ok := e.containers[l.Child]
		if !ok || !child.isRunning() {
			childName := l.Child
			if ok {
				childName = "/" + child.rec.Name
			}
			return nil, invalid("Cannot link to a non running container: %s AS %s", childName, name)
		}
		if child.rec.Networks["bridge"] == nil {
			return nil, invalid("container %s not attached to default bridge network", child.rec.ID)
		}
		out = append(out, link{name: name, alias: l.Alias, child: child})
	}
	return out, nil
}

// linkEnv is the environment Docker gives a container of its legacy
// links: each child's address, exposed ports and environment under the
// link's alias.
func linkEnv(links []link) []string {
	var env []string
	for _, l := range links {
		var ports []network.Port
		for p := range l.child.rec.Config.ExposedPorts {
			ports = append(ports, p)
		}
		env = append(env, linkVars(l.name, l.child.rec.Address, ports, l.child.rec.Config.Env)...)
	}
	return env
}

// linkVars are a link's variables, as Docker writes them: a variable per
// port, and the first and last of each run of consecutive ports of one
// protocol, TCP ports first.
func linkVars(name, addr string, ports []network.Port, childEnv []string) []string {
	alias := strings.ReplaceAll(strings.ToUpper(path.Base(name)), "-", "_")
	sort.Slice(ports, func(i, j int) bool {
		a, b := ports[i], ports[j]
		if a.Proto() == b.Proto() {
			return a.Num() < b.Num()
		}
		if a.Proto() == network.TCP || b.Proto() == network.TCP {
			return a.Proto() == network.TCP
		}
		return a.Proto() < b.Proto()
	})
	var env []string
	var start, end network.Port
	for i, p := range ports {
		proto := string(p.Proto())
		if i == 0 {
			start, end = p, p
			env = append(env, fmt.Sprintf("%s_PORT=%s://%s:%d", alias, proto, addr, p.Num()))
		}
		prefix := fmt.Sprintf("%s_PORT_%d_%s", alias, p.Num(), strings.ToUpper(proto))
		env = append(env,
			fmt.Sprintf("%s=%s://%s:%d", prefix, proto, addr, p.Num()),
			fmt.Sprintf("%s_ADDR=%s", prefix, addr),
			fmt.Sprintf("%s_PORT=%d", prefix, p.Num()),
			fmt.Sprintf("%s_PROTO=%s", prefix, proto))
		if p.Num() == end.Num()+1 && p.Proto() == start.Proto() {
			end = p
			if i < len(ports)-1 {
				continue
			}
		}
		if end != start {
			prefix = fmt.Sprintf("%s_PORT_%d_%s", alias, start.Num(), strings.ToUpper(string(start.Proto())))
			env = append(env,
				fmt.Sprintf("%s_START=%s://%s:%d", prefix, start.Proto(), addr, start.Num()),
				fmt.Sprintf("%s_PORT_START=%d", prefix, start.Num()),
				fmt.Sprintf("%s_END=%s://%s:%d", prefix, end.Proto(), addr, end.Num()),
				fmt.Sprintf("%s_PORT_END=%d", prefix, end.Num()))
		}
		start, end = p, p
	}
	env = append(env, fmt.Sprintf("%s_NAME=%s", alias, name))
	for _, kv := range childEnv {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "HOME" || k == "PATH" {
			continue
		}
		env = append(env, fmt.Sprintf("%s_ENV_%s=%s", alias, k, v))
	}
	return env
}

// linkHosts are the /etc/hosts entries of c's legacy links, as Docker
// writes them: the child's address, by the alias, the child's hostname,
// and its name where the alias differs.
func linkHosts(links []link) []string {
	var out []string
	for _, l := range links {
		names := []string{l.alias, hostnameOf(l.child)}
		if l.alias != l.child.rec.Name {
			names = append(names, l.child.rec.Name)
		}
		out = append(out, l.child.rec.Address+"\t"+strings.Join(names, " "))
	}
	return out
}

func hostnameOf(c *Container) string {
	if c.rec.Config.Hostname != "" {
		return c.rec.Config.Hostname
	}
	return c.rec.ID[:12]
}

// linkLocked finds a link by its name, /<parent>/<alias>: the container
// the parent reaches as alias.
func (e *Engine) linkLocked(ref string) (*Container, *Container, int, bool) {
	parentName, alias, ok := strings.Cut(strings.TrimPrefix(ref, "/"), "/")
	if !ok {
		return nil, nil, 0, false
	}
	for _, p := range e.containers {
		if p.rec.Name != parentName {
			continue
		}
		for i, l := range p.rec.Links {
			if child, ok := e.containers[l.Child]; ok && l.Alias == alias {
				return p, child, i, true
			}
		}
	}
	return nil, nil, 0, false
}

// RemoveLink removes the legacy link named /<parent>/<alias>, as
// docker rm --link does; the parent's running processes keep what they
// were given.
func (e *Engine) RemoveLink(ref string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	name := "/" + strings.TrimPrefix(ref, "/")
	if strings.Count(name, "/") < 2 {
		if _, err := e.containerLocked(ref); err != nil {
			return err
		}
		return fmt.Errorf("Conflict, cannot remove the default link name of the container")
	}
	parent, _, i, ok := e.linkLocked(name)
	if !ok {
		return notFound("No such container: %s", ref)
	}
	parent.mu.Lock()
	parent.rec.Links = append(parent.rec.Links[:i:i], parent.rec.Links[i+1:]...)
	err := parent.save()
	parent.mu.Unlock()
	if err != nil {
		return err
	}
	return e.writeNetworkLocked(parent.rec.Stack)
}

// linkNames are the names Docker lists a container by beyond its own: the
// /<parent>/<alias> of every legacy link to it.
func (e *Engine) linkNames(c *Container) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, p := range e.containers {
		for _, l := range p.rec.Links {
			if l.Child == c.rec.ID {
				out = append(out, "/"+p.rec.Name+"/"+l.Alias)
			}
		}
	}
	sort.Strings(out)
	return out
}

// inspectLinks reports c's legacy links as Docker's inspect does:
// /<child>:/<parent>/<alias>, for each child that still exists.
func (e *Engine) inspectLinks(c *Container) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, l := range c.rec.Links {
		if child, ok := e.containers[l.Child]; ok {
			out = append(out, "/"+child.rec.Name+":/"+c.rec.Name+"/"+l.Alias)
		}
	}
	return out
}

// endpointLinks are the aliases a container on a user-defined network
// gives other containers on it (EndpointSettings.Links), as Docker's DNS
// resolves them for that container alone: for each network, alias to name.
func endpointLinks(c *Container) map[string][][2]string {
	out := map[string][][2]string{}
	for n, s := range c.rec.Networks {
		if s == nil {
			continue
		}
		for _, l := range s.Links {
			if name, alias, err := parseLink(l); err == nil {
				out[n] = append(out[n], [2]string{name, alias})
			}
		}
	}
	return out
}

// checkEndpointLinks refuses a container's network links that Docker's
// start cannot read.
func checkEndpointLinks(c *Container) error {
	for _, s := range c.rec.Networks {
		if s == nil {
			continue
		}
		for _, l := range s.Links {
			if _, _, err := parseLink(l); err != nil {
				return invalid("%v", err)
			}
		}
	}
	return nil
}
