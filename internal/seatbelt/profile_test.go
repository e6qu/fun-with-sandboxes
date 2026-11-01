package seatbelt

import (
	"strings"
	"testing"
)

func spec() Spec {
	return Spec{
		Root:         "/Users/u/.fws/stacks/a/containers/c1/root",
		StateRoot:    "/Users/u/.fws",
		Volumes:      []Volume{{Path: "/Users/u/.fws/stacks/a/volumes/data"}, {Path: "/Users/u/.fws/stacks/a/volumes/conf", ReadOnly: true}},
		BindPorts:    []int{41000},
		ConnectPorts: []int{41001, 41000, 41001},
	}
}

func TestRenderGrantsOnlyOwnResources(t *testing.T) {
	p, err := Render(spec())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`(allow file-write*` + "\n" + `  (subpath "/Users/u/.fws/stacks/a/containers/c1/root")`,
		`(subpath "/Users/u/.fws/stacks/a/volumes/data")`,
		`(allow network-bind (local ip "localhost:41000"))`,
		`(allow network-outbound (remote ip "localhost:41000"))`,
		`(allow network-outbound (remote ip "localhost:41001"))`,
		`(allow process-exec*` + "\n" + `  (subpath "/Users/u/.fws/stacks/a/containers/c1/root")`,
		`  (literal "/bin/sh")`,
		`(allow signal (target same-sandbox))`,
		`(literal "/Users/u/.fws/stacks/a/containers")`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %s\n%s", want, p)
		}
	}
	if strings.Count(p, `"localhost:41001"`) != 1 {
		t.Errorf("duplicate connect port rendered twice")
	}
	if strings.Contains(p, `"localhost:41001"))`+"\n"+`(allow network-bind`) {
		t.Errorf("connect-only port granted bind")
	}
}

func TestReadOnlyVolumeIsNotWritable(t *testing.T) {
	p, _ := Render(spec())
	writes := p[strings.Index(p, "(allow file-write*"):strings.Index(p, "(deny file-read*")]
	if strings.Contains(writes, "volumes/conf") {
		t.Fatalf("read-only volume appears in write allowances:\n%s", writes)
	}
	if !strings.Contains(p, `(subpath "/Users/u/.fws/stacks/a/volumes/conf")`) {
		t.Fatalf("read-only volume not readable")
	}
}

func TestStateRootDeniedThenRootReallowed(t *testing.T) {
	p, _ := Render(spec())
	deny := strings.Index(p, `(deny file-read*`)
	allow := strings.Index(p, `(allow file-read*`)
	if deny < 0 || allow < deny {
		t.Fatalf("read allowance must follow the denial so it wins")
	}
	if !strings.Contains(p[deny:allow], `(subpath "/Users/u/.fws")`) {
		t.Fatalf("engine state root not denied")
	}
}

func TestRejectsUnsafePaths(t *testing.T) {
	for _, root := range []string{"relative", `/a"b`, `/a\b`, "/a\nb"} {
		s := spec()
		s.Root = root
		if _, err := Render(s); err == nil {
			t.Errorf("Render accepted root %q", root)
		}
	}
	s := spec()
	s.BindPorts = []int{70000}
	if _, err := Render(s); err == nil {
		t.Errorf("Render accepted port 70000")
	}
}
