package engine

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/alloc"
)

func TestParseLink(t *testing.T) {
	for in, want := range map[string][2]string{
		"db":           {"db", "db"},
		"db:database":  {"db", "database"},
		"/db:/web/dbx": {"db", "dbx"},
	} {
		name, alias, err := parseLink(in)
		if err != nil || name != want[0] || alias != want[1] {
			t.Errorf("parseLink(%q) = %q, %q, %v", in, name, alias, err)
		}
	}
	for _, in := range []string{"", "a:b:c"} {
		if _, _, err := parseLink(in); err == nil {
			t.Errorf("parseLink(%q) accepted it", in)
		}
	}
}

// The variables match Docker's own for its test case of several ports.
func TestLinkVars(t *testing.T) {
	var ports []network.Port
	for _, p := range []string{"6300/udp", "6379/tcp", "6380/tcp", "6381/tcp", "6382/udp"} {
		ports = append(ports, network.MustParsePort(p))
	}
	got := linkVars("/db/docker", "172.0.17.2", ports, []string{"PASSWORD=gordon", "HOME=/root", "PATH=/bin"})
	sort.Strings(got)
	want := []string{
		"DOCKER_ENV_PASSWORD=gordon",
		"DOCKER_NAME=/db/docker",
		"DOCKER_PORT=tcp://172.0.17.2:6379",
		"DOCKER_PORT_6300_UDP=udp://172.0.17.2:6300",
		"DOCKER_PORT_6300_UDP_ADDR=172.0.17.2",
		"DOCKER_PORT_6300_UDP_PORT=6300",
		"DOCKER_PORT_6300_UDP_PROTO=udp",
		"DOCKER_PORT_6379_TCP=tcp://172.0.17.2:6379",
		"DOCKER_PORT_6379_TCP_ADDR=172.0.17.2",
		"DOCKER_PORT_6379_TCP_END=tcp://172.0.17.2:6381",
		"DOCKER_PORT_6379_TCP_PORT=6379",
		"DOCKER_PORT_6379_TCP_PORT_END=6381",
		"DOCKER_PORT_6379_TCP_PORT_START=6379",
		"DOCKER_PORT_6379_TCP_PROTO=tcp",
		"DOCKER_PORT_6379_TCP_START=tcp://172.0.17.2:6379",
		"DOCKER_PORT_6380_TCP=tcp://172.0.17.2:6380",
		"DOCKER_PORT_6380_TCP_ADDR=172.0.17.2",
		"DOCKER_PORT_6380_TCP_PORT=6380",
		"DOCKER_PORT_6380_TCP_PROTO=tcp",
		"DOCKER_PORT_6381_TCP=tcp://172.0.17.2:6381",
		"DOCKER_PORT_6381_TCP_ADDR=172.0.17.2",
		"DOCKER_PORT_6381_TCP_PORT=6381",
		"DOCKER_PORT_6381_TCP_PROTO=tcp",
		"DOCKER_PORT_6382_UDP=udp://172.0.17.2:6382",
		"DOCKER_PORT_6382_UDP_ADDR=172.0.17.2",
		"DOCKER_PORT_6382_UDP_PORT=6382",
		"DOCKER_PORT_6382_UDP_PROTO=udp",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("linkVars:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := linkVars("/web/my-db", "127.88.0.2", nil, nil); !reflect.DeepEqual(got, []string{"MY_DB_NAME=/web/my-db"}) {
		t.Errorf("linkVars without ports: %q", got)
	}
}

func TestLinks(t *testing.T) {
	a, err := alloc.Open(alloc.Config{Root: t.TempDir(), PortBase: 30000, PortLimit: 31000, BlockSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{alloc: a, containers: map[string]*Container{}}
	add := func(stack, id, name string, ports map[string]int, nets ...string) *Container {
		addr, err := e.assignAddressLocked(stack)
		if err != nil {
			t.Fatal(err)
		}
		c := &Container{dir: t.TempDir(), rec: record{ID: id + strings.Repeat("0", 64-len(id)), Name: name, Service: name, Stack: stack, Ports: ports, Address: addr,
			RouterPort: 30050 + len(e.containers), HostConfig: container.HostConfig{NetworkMode: container.NetworkMode(nets[0])},
			Config:   container.Config{Env: []string{"POSTGRES_DB=app"}, ExposedPorts: network.PortSet{network.MustParsePort("5432/tcp"): {}}},
			State:    stateRec{Status: container.StateRunning},
			Networks: map[string]*network.EndpointSettings{}}}
		for _, n := range nets {
			c.rec.Networks[n] = &network.EndpointSettings{}
		}
		e.containers[c.rec.ID] = c
		return c
	}
	// Two docker run containers, each its own stack, on the default bridge.
	db := add("run-db", "aaaa", "db", map[string]int{"5432/tcp": 30010}, "bridge")
	sidecar := add("run-sc", "ffff", "sidecar", nil, "container:"+db.rec.ID)
	links, err := e.legacyLinksLocked(&container.HostConfig{Links: []string{"db:database", "sidecar", "db:database"}})
	if err != nil {
		t.Fatal(err)
	}
	// A link to a container on another's network links to that other, and
	// a repeated alias is dropped.
	if want := []linkRec{{db.rec.ID, "database"}, {db.rec.ID, "sidecar"}}; !reflect.DeepEqual(links, want) {
		t.Errorf("links: %+v", links)
	}
	delete(e.containers, sidecar.rec.ID)
	if _, err := e.legacyLinksLocked(&container.HostConfig{Links: []string{"nothing"}}); err == nil || KindOf(err) != KindInvalid {
		t.Errorf("a link to no container: %v", err)
	}
	if l, _ := e.legacyLinksLocked(&container.HostConfig{NetworkMode: "proj_default", Links: []string{"db"}}); l != nil {
		t.Errorf("legacy links on a user-defined network: %+v", l)
	}

	web := add("run-web", "bbbb", "web", nil, "bridge")
	web.rec.Links = links[:1]
	lone := add("run-lone", "cccc", "lone", nil, "bridge")
	if err := e.writeNetworkLocked("run-web"); err != nil {
		t.Fatal(err)
	}
	read := func(stack string) string {
		p, _ := e.networkFile(stack)
		b, _ := os.ReadFile(p)
		return string(b)
	}
	// The linking container's stack lists the child, with the alias its
	// viewer knows it by; another stack on the bridge does not see it.
	want := web.rec.Address + "\tweb\t\t30051\tbridge\t0\t\t\n" +
		db.rec.Address + "\tdb\t5432/tcp=30010\t30050\t\t1\trun-db\t" + web.rec.Address + "/database\n"
	if got := read("run-web"); got != want {
		t.Errorf("run-web's network file:\n%s\nwant:\n%s", got, want)
	}
	if got := read("run-lone"); strings.Contains(got, db.rec.Address) {
		t.Errorf("run-lone sees the linked container:\n%s", got)
	}

	ls, err := e.linksOf(web)
	if err != nil {
		t.Fatal(err)
	}
	env := linkEnv(ls)
	for _, kv := range []string{"DATABASE_PORT=tcp://" + db.rec.Address + ":5432", "DATABASE_NAME=/web/database", "DATABASE_ENV_POSTGRES_DB=app"} {
		if !contains(env, kv) {
			t.Errorf("link env lacks %s: %q", kv, env)
		}
	}
	if got := linkHosts(ls); !reflect.DeepEqual(got, []string{db.rec.Address + "\tdatabase " + db.rec.ID[:12] + " db"}) {
		t.Errorf("link hosts: %q", got)
	}
	if got := e.linkNames(db); !reflect.DeepEqual(got, []string{"/web/database"}) {
		t.Errorf("link names: %q", got)
	}
	if got := e.inspectLinks(web); !reflect.DeepEqual(got, []string{"/db:/web/database"}) {
		t.Errorf("inspect links: %q", got)
	}
	if c, err := e.Container("web/database"); err != nil || c != db {
		t.Errorf("the link name resolves to %v, %v", c, err)
	}
	// Start fails on a child that is not running.
	db.rec.State.Status = container.StateExited
	if _, err := e.linksOf(web); err == nil || !strings.Contains(err.Error(), "Cannot link to a non running container: /db AS /web/database") {
		t.Errorf("a stopped child: %v", err)
	}
	db.rec.State.Status = container.StateRunning
	// Off the default bridge, a container's links have no effect.
	if ls, err := e.linksOf(lone); err != nil || ls != nil {
		t.Errorf("links of a container without any: %v, %v", ls, err)
	}

	if err := e.RemoveLink("db"); err == nil || !strings.Contains(err.Error(), "cannot remove the default link name") {
		t.Errorf("removing a container's own name as a link: %v", err)
	}
	if err := e.RemoveLink("/web/database"); err != nil {
		t.Fatal(err)
	}
	if len(web.rec.Links) != 0 || strings.Contains(read("run-web"), "/database") {
		t.Errorf("the link stayed: %+v\n%s", web.rec.Links, read("run-web"))
	}
	if err := e.RemoveLink("/web/database"); KindOf(err) != KindNotFound {
		t.Errorf("removing a removed link: %v", err)
	}
}

// A link on a user-defined network gives the container that makes it an
// alias of its own for the container on the network answering to the name.
func TestEndpointLinks(t *testing.T) {
	a, err := alloc.Open(alloc.Config{Root: t.TempDir(), PortBase: 30000, PortLimit: 31000, BlockSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{alloc: a, containers: map[string]*Container{}}
	add := func(id, name string, links ...string) *Container {
		addr, _ := e.assignAddressLocked("proj")
		c := &Container{rec: record{ID: id, Name: name, Service: name, Stack: "proj", Address: addr, RouterPort: 30060 + len(e.containers),
			HostConfig: container.HostConfig{NetworkMode: "proj_default"},
			Networks:   map[string]*network.EndpointSettings{"proj_default": {Links: links}}}}
		e.containers[id] = c
		return c
	}
	db := add("aaaa", "proj-db-1")
	web := add("bbbb", "proj-web-1", "proj-db-1:database")
	if err := e.writeNetworkLocked("proj"); err != nil {
		t.Fatal(err)
	}
	p, _ := e.networkFile("proj")
	b, _ := os.ReadFile(p)
	if want := db.rec.Address + "\tproj-db-1\t\t30060\tproj_default\t0\t\t" + web.rec.Address + "/database\n"; !strings.HasPrefix(string(b), want) {
		t.Errorf("network file:\n%s\nwant first line:\n%s", b, want)
	}
	web.rec.Networks["proj_default"].Links = []string{"a:b:c"}
	if err := checkEndpointLinks(web); err == nil {
		t.Error("a malformed network link was accepted")
	}
}
