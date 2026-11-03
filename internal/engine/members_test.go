package engine

import (
	"os"
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
	add := func(id, name, service string, ports map[string]int, hc container.HostConfig) *Container {
		addr, err := e.assignAddressLocked("proj")
		if err != nil {
			t.Fatal(err)
		}
		router := 30020 + len(e.containers)
		c := &Container{rec: record{ID: id, Name: name, Service: service, Stack: "proj", Ports: ports, HostConfig: hc, Address: addr, RouterPort: router,
			Config:   container.Config{Hostname: id[:4]},
			Networks: map[string]*network.EndpointSettings{"proj_default": {Aliases: []string{"database"}, DNSNames: []string{name, service}}}}}
		e.containers[id] = c
		return c
	}
	db := add("aaaa1111", "proj-db-1", "db", map[string]int{"5432/tcp": 30010}, container.HostConfig{})
	add("bbbb2222", "proj-web-1", "web", map[string]int{"80/tcp": 30011, "53/udp": 30012}, container.HostConfig{})
	add("cccc3333", "proj-job-1", "job", nil, container.HostConfig{NetworkMode: "none"})
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
	want := "127.88.0.2\tproj-db-1 db aaaa database\t5432/tcp=30010\t30020\n" +
		"127.88.0.3\tproj-web-1 web bbbb database\t53/udp=30012,80/tcp=30011\t30021\n"
	if string(b) != want {
		t.Errorf("network file:\n%s\nwant:\n%s", b, want)
	}
	// A removed member's address is free again.
	delete(e.containers, db.rec.ID)
	if a, _ := e.assignAddressLocked("proj"); a != "127.88.0.2" {
		t.Errorf("after a removal the next address is %q", a)
	}
}
