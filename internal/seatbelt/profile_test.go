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
		IPCPrefix:    IPCPrefixFor("c1"),
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
		`(allow process-exec* process-exec-interpreter` + "\n" + `  (subpath "/Users/u/.fws/stacks/a/containers/c1/root")` + "\n" + `  (subpath "/Users/u/.fws/stacks/a/volumes/data")`,
		`  (literal "/bin/sh")`,
		`(allow signal (target same-sandbox))`,
		`  (global-name "com.apple.trustd")`,
		`(allow file-read-metadata` + "\n" + `  (literal "/")`,
		`  (literal "/Users/u/.fws/stacks/a/containers")`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %s\n%s", want, p)
		}
	}
	if strings.Contains(p, "com.apple.SecurityServer") {
		t.Errorf("profile grants the keychain service")
	}
	if strings.Count(p, `"localhost:41001"`) != 1 {
		t.Errorf("duplicate connect port rendered twice")
	}
	if strings.Contains(p, `"localhost:41001"))`+"\n"+`(allow network-bind`) {
		t.Errorf("connect-only port granted bind")
	}
}

func TestAncestorsAreMetadataOnly(t *testing.T) {
	p, _ := Render(spec())
	reads := p[strings.Index(p, "(allow file-read*"):strings.Index(p, "(allow file-read-metadata")]
	if strings.Contains(reads, `(literal "/Users/u/.fws/stacks/a/containers")`) {
		t.Fatalf("ancestor granted full reads, so a container could list it:\n%s", reads)
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
	allow := deny + strings.Index(p[deny:], `(allow file-read*`)
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

// Unix sockets in a mount are reachable, and creatable in a writable one.
func TestMountedUnixSockets(t *testing.T) {
	p, _ := Render(spec())
	for _, want := range []string{
		`(allow network-outbound (remote unix-socket (subpath "/Users/u/.fws/stacks/a/volumes/data")))`,
		`(allow network-outbound (remote unix-socket (subpath "/Users/u/.fws/stacks/a/volumes/conf")))`,
		`(allow network-bind (local unix-socket (subpath "/Users/u/.fws/stacks/a/volumes/data")))`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %s", want)
		}
	}
	if strings.Contains(p, `(allow network-bind (local unix-socket (subpath "/Users/u/.fws/stacks/a/volumes/conf")))`) {
		t.Error("a read-only mount allows making sockets")
	}
}

func TestRenderDeniesByDefault(t *testing.T) {
	p, err := Render(spec())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, "(allow default)") || !strings.Contains(p, "(deny default)\n(import \"system.sb\")") {
		t.Fatalf("profile does not deny by default on Apple's baseline:\n%s", p)
	}
	if !strings.Contains(p, "(deny system-socket (socket-domain 32) (socket-domain 27))") {
		t.Errorf("profile allows kernel control or link-layer sockets")
	}
}

func TestLibrariesReadableAndMappable(t *testing.T) {
	s := spec()
	s.Libraries = []string{"/Users/u/.fws/lib/fwsroot-1.dylib"}
	p, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	deny := strings.Index(p, `(deny file-read*`)
	lib := strings.Index(p, `(literal "/Users/u/.fws/lib/fwsroot-1.dylib")`)
	if lib < deny || !strings.Contains(p, "(allow file-map-executable\n  (literal \"/Users/u/.fws/lib/fwsroot-1.dylib\")") {
		t.Fatalf("library not granted after the state root's denial:\n%s", p)
	}
	s.Libraries = []string{"relative.dylib"}
	if _, err := Render(s); err == nil {
		t.Fatal("a relative library path was accepted")
	}
}

func TestReadFilesReadableNotMappable(t *testing.T) {
	s := spec()
	s.ReadFiles = []string{"/Users/u/.fws/stacks/a/network"}
	p, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	deny := strings.Index(p, `(deny file-read*`)
	f := strings.Index(p, `(literal "/Users/u/.fws/stacks/a/network")`)
	if f < deny || strings.Count(p, `(literal "/Users/u/.fws/stacks/a/network")`) != 1 {
		t.Fatalf("network file not readable after the state root's denial, or mappable:\n%s", p)
	}
	s.ReadFiles = []string{"rel"}
	if _, err := Render(s); err == nil {
		t.Fatal("a relative path was accepted")
	}
}

func TestMachServicesUnderTheIPCPrefix(t *testing.T) {
	s := spec()
	p, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`(allow mach-register (global-name-prefix "` + s.IPCPrefix + `"))`,
		`  (global-name-prefix "` + s.IPCPrefix + `")`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %s", want)
		}
	}
	s.IPC = IPCNone
	if p, _ := Render(s); strings.Contains(p, "mach-register") {
		t.Errorf("--ipc none lets a container register Mach services:\n%s", p)
	}
}

func TestHelperExecutable(t *testing.T) {
	s := spec()
	s.Helper = "/usr/local/bin/fws-shim"
	p, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	exec := p[strings.Index(p, "(allow process-exec*"):]
	exec = exec[:strings.Index(exec, ")\n(")]
	if !strings.Contains(exec, `(literal "/usr/local/bin/fws-shim")`) {
		t.Fatalf("the helper is not executable:\n%s", p)
	}
	s.Helper = "fws-shim"
	if _, err := Render(s); err == nil {
		t.Fatal("a relative helper path was accepted")
	}
}

func TestPortDirForUnixListeners(t *testing.T) {
	s := spec()
	s.PortDir = "/Users/u/.fws/run/abc.p"
	p, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		`(allow network-bind (local unix-socket (subpath "/Users/u/.fws/run/abc.p")))`,
		`(allow network-outbound (remote unix-socket (subpath "/Users/u/.fws/run/abc.p")))`,
	} {
		if !strings.Contains(p, rule) {
			t.Errorf("missing %s:\n%s", rule, p)
		}
	}
	if strings.Count(p, `(subpath "/Users/u/.fws/run/abc.p")`) < 4 {
		t.Errorf("the port directory is not readable and writable:\n%s", p)
	}
}

// On the host's PID namespace a container signals and inspects any
// process; otherwise its own sandbox's alone.
func TestHostPIDs(t *testing.T) {
	s := spec()
	s.HostPIDs = true
	p, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(allow signal)\n", "(allow process-info*)\n"} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %s\n%s", want, p)
		}
	}
	if strings.Contains(p, "(target same-sandbox)") || strings.Contains(p, "(deny signal)") {
		t.Errorf("a host PID namespace's profile confines processes to the sandbox:\n%s", p)
	}
}

// A container reaches its shim's process socket, by its path alone.
func TestProcsSocketIsReachable(t *testing.T) {
	s := spec()
	s.ProcsSocket = "/Users/u/.fws/run/c1.procs"
	p, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, `(allow network-outbound (remote unix-socket (path-literal "/Users/u/.fws/run/c1.procs")))`) {
		t.Errorf("profile does not reach the process socket:\n%s", p)
	}
	if strings.Contains(p, `(subpath "/Users/u/.fws/run")`) {
		t.Errorf("profile grants the run directory:\n%s", p)
	}
}

// A privileged container reaches its nested engine's socket, and writes
// its shared memory ledger, by their paths alone.
func TestSocketsAndWriteFilesAreReachable(t *testing.T) {
	s := spec()
	s.Sockets = []string{"/Users/u/.fws/run/c1.engine"}
	s.WriteFiles = []string{"/Users/u/.fws/stacks/x/c/c1/shm"}
	p, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`(allow network-outbound (remote unix-socket (path-literal "/Users/u/.fws/run/c1.engine")))`,
		`  (literal "/Users/u/.fws/stacks/x/c/c1/shm")`} {
		if !strings.Contains(p, want) {
			t.Errorf("profile lacks %s:\n%s", want, p)
		}
	}
	s.Sockets = []string{`/bad"path`}
	if _, err := Render(s); err == nil {
		t.Error("a socket path with a quote rendered")
	}
}
