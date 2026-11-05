package engine

import (
	"os"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/alloc"
)

func TestMembersAndTheirNetworkFile(t *testing.T) {
	a, err := alloc.Open(alloc.Config{Root: t.TempDir(), PortBase: 30000, PortLimit: 31000, BlockSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{alloc: a, containers: map[string]*Container{}}
	addIn := func(stack, id, name, service string, ports map[string]int, hc container.HostConfig, nets ...string) *Container {
		addr, err := e.assignAddressLocked(stack)
		if err != nil {
			t.Fatal(err)
		}
		router := 30020 + len(e.containers)
		c := &Container{rec: record{ID: id, Name: name, Service: service, Stack: stack, Ports: ports, HostConfig: hc, Address: addr, RouterPort: router,
			Config:   container.Config{Hostname: id[:4]},
			Networks: map[string]*network.EndpointSettings{}}}
		for _, n := range nets {
			c.rec.Networks[n] = &network.EndpointSettings{Aliases: []string{"database"}, DNSNames: []string{name, service}}
		}
		e.containers[id] = c
		return c
	}
	add := func(id, name, service string, ports map[string]int, hc container.HostConfig, nets ...string) *Container {
		return addIn("proj", id, name, service, ports, hc, append(nets, "proj_default")...)
	}
	db := add("aaaa1111", "proj-db-1", "db", map[string]int{"5432/tcp": 30010}, container.HostConfig{})
	add("bbbb2222", "proj-web-1", "web", map[string]int{"80/tcp": 30011, "53/udp": 30012}, container.HostConfig{}, "shared")
	add("cccc3333", "proj-job-1", "job", nil, container.HostConfig{NetworkMode: "none"})
	// Another stack's members: one on the network the stacks share, which
	// each stack's file lists as foreign, and one on its own network alone.
	addIn("other", "dddd4444", "other-api-1", "api", map[string]int{"8000/tcp": 30110}, container.HostConfig{}, "other_default", "shared")
	addIn("other", "eeee5555", "other-cache-1", "cache", nil, container.HostConfig{}, "other_default", "bridge")
	if db.rec.Address != "127.88.0.2" {
		t.Errorf("the first member's address: %q", db.rec.Address)
	}
	if err := e.writeNetworkLocked("proj"); err != nil {
		t.Fatal(err)
	}
	path, _ := e.networkFile("proj")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "127.88.0.2\tproj-db-1 db aaaa database\t5432/tcp=30010\t30020\tproj_default\t0\t\n" +
		"127.88.0.3\tproj-web-1 web bbbb database\t53/udp=30012,80/tcp=30011\t30021\tproj_default shared\t0\t\n" +
		"127.88.0.5\tother-api-1 api dddd database\t8000/tcp=30110\t30023\tother_default shared\t1\tother\n"
	if string(b) != want {
		t.Errorf("network file:\n%s\nwant:\n%s", b, want)
	}
	// Writing one stack's file writes the others', whose members see the
	// first stack's on the network they share.
	path, _ = e.networkFile("other")
	if b, err = os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	want = "127.88.0.3\tproj-web-1 web bbbb database\t53/udp=30012,80/tcp=30011\t30021\tproj_default shared\t1\tproj\n"
	if !strings.HasPrefix(string(b), "127.88.0.5\t") || !strings.HasSuffix(string(b), want) || strings.Count(string(b), "\n") != 3 {
		t.Errorf("the other stack's network file:\n%s", b)
	}
	// A removed member's address is free again.
	delete(e.containers, db.rec.ID)
	if a, _ := e.assignAddressLocked("other"); a != "127.88.0.2" {
		t.Errorf("after a removal the next address is %q", a)
	}
}
