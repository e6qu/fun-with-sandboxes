// Package containerd reads the containerd configuration a kind node holds,
// as kind's containerdConfigPatches change it, for what the node's engine
// takes from it in containerd's place: the registries an image is pulled
// through, as registry mirrors and a hosts directory (config_path) name
// them. A node runs no containerd; any other setting a patch makes is
// one the engine does not apply, and Parse lists it.
package containerd

import (
	"bufio"
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// ConfigPath is where the node's configuration lies, where kind reads and
// patches it.
const ConfigPath = "/etc/containerd/config.toml"

// Config is the configuration fws/kind-node starts with, which
// containerd config dump prints: kind reads the snapshotter it loads
// images with from it.
const Config = `version = 2

# fws/kind-node runs no containerd: its runtime is the engine fwsd nests in
# the node, which serves the CRI at /run/containerd/containerd.sock. Of
# what kind's containerdConfigPatches set, the engine takes the registries
# an image is pulled through: [plugins."io.containerd.grpc.v1.cri".registry]
# mirrors and config_path. It reports any other setting as one it does not
# apply.
[plugins."io.containerd.grpc.v1.cri".containerd]
  snapshotter = "native"
`

// criPlugins are the plugins whose registry settings name the registries:
// containerd 1's and 2's.
var criPlugins = []string{"io.containerd.grpc.v1.cri", "io.containerd.cri.v1.images"}

// Endpoint is a registry an image is pulled through: its host and port,
// whether it speaks plain HTTP, and whether its certificate goes
// unchecked.
type Endpoint struct {
	Host       string
	Insecure   bool
	SkipVerify bool
}

// Settings are what a node's configuration asks of the engine.
type Settings struct {
	// Mirrors are each registry's mirrors, in order.
	Mirrors map[string][]Endpoint
	// HostsDir is the directory of hosts.toml files, a registry's at
	// <HostsDir>/<registry>/hosts.toml.
	HostsDir string
	// Unsupported are the settings, by their dotted keys, the engine does
	// not apply.
	Unsupported []string
}

// Parse reads configuration text.
func Parse(text []byte) (Settings, error) {
	var doc, base map[string]any
	if err := toml.Unmarshal(text, &doc); err != nil {
		return Settings{}, fmt.Errorf("%s: %v", ConfigPath, err)
	}
	if err := toml.Unmarshal([]byte(Config), &base); err != nil {
		return Settings{}, err
	}
	baseLeaves := map[string]any{}
	flatten(nil, base, func(path []string, v any) { baseLeaves[key(path)] = v })
	s := Settings{Mirrors: map[string][]Endpoint{}}
	var problems []string
	flatten(nil, doc, func(path []string, v any) {
		if b, ok := baseLeaves[key(path)]; ok && reflect.DeepEqual(b, v) {
			return
		}
		if len(path) >= 4 && path[0] == "plugins" && contains(criPlugins, path[1]) && path[2] == "registry" {
			switch {
			case len(path) == 4 && path[3] == "config_path":
				if dir, ok := v.(string); ok {
					s.HostsDir = dir
					return
				}
			case len(path) == 6 && path[3] == "mirrors" && path[5] == "endpoint":
				if list, ok := v.([]any); ok {
					var eps []Endpoint
					bad := false
					for _, e := range list {
						str, ok := e.(string)
						ep, err := parseEndpoint(str)
						if !ok || err != nil {
							bad = true
							break
						}
						eps = append(eps, ep)
					}
					if !bad {
						s.Mirrors[path[4]] = eps
						return
					}
				}
			}
		}
		problems = append(problems, key(path))
	})
	sort.Strings(problems)
	s.Unsupported = problems
	return s, nil
}

// flatten calls leaf with the path of each value of doc that is not a
// table.
func flatten(path []string, doc map[string]any, leaf func([]string, any)) {
	for k, v := range doc {
		p := append(append([]string(nil), path...), k)
		if t, ok := v.(map[string]any); ok {
			flatten(p, t, leaf)
			continue
		}
		leaf(p, v)
	}
}

// key is a path as a dotted TOML key, a part with a dot or other
// character a bare key may not hold quoted.
func key(path []string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		if bareKey.MatchString(p) {
			parts[i] = p
		} else {
			parts[i] = fmt.Sprintf("%q", p)
		}
	}
	return strings.Join(parts, ".")
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func contains(list []string, s string) bool {
	for _, l := range list {
		if l == s {
			return true
		}
	}
	return false
}

// parseEndpoint reads a registry's URL, or its host alone, which speaks
// HTTPS.
func parseEndpoint(s string) (Endpoint, error) {
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Endpoint{}, fmt.Errorf("registry endpoint %q is not an http or https URL", s)
	}
	return Endpoint{Host: u.Host, Insecure: u.Scheme == "http"}, nil
}

// hostHeader matches a hosts.toml table that names a host.
var hostHeader = regexp.MustCompile(`^\s*\[\s*host\s*\.\s*"([^"]+)"\s*\]`)

// Hosts reads the hosts.toml of registry in dir, as containerd does, or,
// where it has none, _default's: the hosts it names that pull, in their
// order, then its server, where one is named. A registry with no file has
// none.
func Hosts(read func(string) ([]byte, error), dir, registry string) ([]Endpoint, error) {
	var b []byte
	var err error
	for _, name := range []string{registry, "_default"} {
		if b, err = read(filepath.Join(dir, name, "hosts.toml")); err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if err != nil {
		return nil, nil
	}
	var doc struct {
		Server string `toml:"server"`
		Host   map[string]struct {
			Capabilities []string `toml:"capabilities"`
			SkipVerify   bool     `toml:"skip_verify"`
		} `toml:"host"`
	}
	if err := toml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s/%s/hosts.toml: %v", dir, registry, err)
	}
	var out []Endpoint
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		m := hostHeader.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		h, ok := doc.Host[m[1]]
		if !ok || (h.Capabilities != nil && !contains(h.Capabilities, "pull")) {
			continue
		}
		ep, err := parseEndpoint(m[1])
		if err != nil {
			return nil, err
		}
		ep.SkipVerify = h.SkipVerify
		out = append(out, ep)
	}
	if doc.Server != "" {
		ep, err := parseEndpoint(doc.Server)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, nil
}
