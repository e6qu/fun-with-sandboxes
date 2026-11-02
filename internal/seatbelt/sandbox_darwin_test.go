package seatbelt

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

type probeEnv struct {
	t       *testing.T
	root    string
	profile string
}

func newProbeEnv(t *testing.T, s Spec) *probeEnv {
	t.Helper()
	build := exec.Command("go", "build", "-o", filepath.Join(s.Root, "probe"), "./testdata/probe")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v\n%s", err, out)
	}
	p, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	return &probeEnv{t: t, root: s.Root, profile: p}
}

func (e *probeEnv) run(op, arg string) error {
	cmd := exec.Command("/usr/bin/sandbox-exec", "-p", e.profile, filepath.Join(e.root, "probe"), op, arg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Logf("probe %s %s: %v: %s", op, arg, err, out)
	}
	return err
}

func (e *probeEnv) allowed(op, arg string) {
	e.t.Helper()
	if err := e.run(op, arg); err != nil {
		e.t.Errorf("%s %s was refused, want allowed", op, arg)
	}
}

func (e *probeEnv) denied(op, arg string) {
	e.t.Helper()
	if err := e.run(op, arg); err == nil {
		e.t.Errorf("%s %s succeeded, want denied", op, arg)
	}
}

func listen(t *testing.T) (*net.TCPListener, int) {
	t.Helper()
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return l, l.Addr().(*net.TCPAddr).Port
}

func freePort(t *testing.T) int {
	t.Helper()
	l, p := listen(t)
	l.Close()
	return p
}

func TestSandboxEnforcesSpec(t *testing.T) {
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(state, "stacks", "a", "containers", "c1", "root")
	vol := filepath.Join(state, "stacks", "a", "volumes", "data")
	ro := filepath.Join(state, "stacks", "a", "volumes", "conf")
	other := filepath.Join(state, "stacks", "b", "containers", "c2", "root")
	for _, d := range []string{root, vol, ro, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(other, "secret")
	os.WriteFile(secret, []byte("b's data"), 0o600)
	os.WriteFile(filepath.Join(ro, "conf"), []byte("c"), 0o600)
	home, _ := os.UserHomeDir()

	_, peer := listen(t)
	_, foreign := listen(t)
	bindOK := freePort(t)
	bindNo := freePort(t)

	e := newProbeEnv(t, Spec{
		IPCPrefix:    IPCPrefixFor("enforce"),
		Root:         root,
		StateRoot:    state,
		Volumes:      []Volume{{Path: vol}, {Path: ro, ReadOnly: true}},
		BindPorts:    []int{bindOK},
		ConnectPorts: []int{peer},
	})

	t.Run("filesystem", func(t *testing.T) {
		e.t = t
		e.allowed("write", filepath.Join(root, "f"))
		e.allowed("write", filepath.Join(vol, "f"))
		e.allowed("read", filepath.Join(ro, "conf"))
		e.denied("write", filepath.Join(ro, "f"))
		e.denied("write", filepath.Join(other, "f"))
		e.denied("read", secret)
		e.denied("list", other)
		e.denied("list", filepath.Dir(root))
		e.denied("list", state)
		e.allowed("list", root)
		e.denied("list", home)
		e.denied("write", "/private/tmp/fws-escape")
		e.allowed("read", "/System/Library/CoreServices/SystemVersion.plist")
	})
	t.Run("network", func(t *testing.T) {
		e.t = t
		e.allowed("bind", strconv.Itoa(bindOK))
		e.denied("bind", strconv.Itoa(bindNo))
		e.allowed("connect", strconv.Itoa(peer))
		e.denied("connect", strconv.Itoa(foreign))
	})
	t.Run("processes", func(t *testing.T) {
		e.t = t
		e.denied("signal", strconv.Itoa(os.Getpid()))
		e.denied("exec", "/bin/ls")
		e.allowed("exec", "/bin/sh")
		e.allowed("exec", "/usr/bin/env")
		// A script in the root, and one in a mount, run under their
		// interpreter.
		for _, dir := range []string{root, vol} {
			script := filepath.Join(dir, "script")
			if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			e.allowed("exec", script)
		}
	})
}

func probeRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// The profile denies by default: a container opens the sockets an ordinary
// program needs, and no kernel control or link-layer ones.
func TestSocketDomains(t *testing.T) {
	root := probeRoot(t)
	e := newProbeEnv(t, Spec{Root: root, StateRoot: filepath.Dir(root), IPCPrefix: IPCPrefixFor(root)})
	for _, k := range []string{"unix", "inet", "inet6", "route", "icmp", "icmp6"} {
		e.allowed("socket", k)
	}
	for _, k := range []string{"system", "ndrv"} {
		e.denied("socket", k)
	}
	e.allowed("sh", "echo ok")
}

// ICMP reaches nothing: a container's outbound traffic is held to its
// stack's ports, and ICMP has none, so an ICMP datagram socket opens but
// sends nowhere, not even to loopback.
func TestICMPReachesNothing(t *testing.T) {
	root := probeRoot(t)
	e := newProbeEnv(t, Spec{Root: root, StateRoot: filepath.Dir(root), IPCPrefix: IPCPrefixFor(root)})
	e.allowed("socket", "icmp")
	e.denied("ping", "127.0.0.1")
}

func TestReadOnlyRoot(t *testing.T) {
	root := probeRoot(t)
	vol := filepath.Join(filepath.Dir(root), "vol")
	os.MkdirAll(vol, 0o755)
	e := newProbeEnv(t, Spec{Root: root, StateRoot: filepath.Dir(root), IPCPrefix: IPCPrefixFor(root), ReadOnlyRoot: true, Volumes: []Volume{{Path: vol}}})
	e.denied("write", filepath.Join(root, "f"))
	e.allowed("read", filepath.Join(root, "probe"))
	e.allowed("write", filepath.Join(vol, "f"))
}

func TestIPCModes(t *testing.T) {
	root := probeRoot(t)
	prefix := IPCPrefixFor(root)
	own, other := prefix+strconv.Itoa(os.Getpid()), "fwsother."+strconv.Itoa(os.Getpid())
	key := strconv.Itoa(0x46570000 + os.Getpid()%0xffff)
	spec := Spec{Root: root, StateRoot: filepath.Dir(root), IPCPrefix: prefix}
	t.Run("private", func(t *testing.T) {
		e := newProbeEnv(t, spec)
		e.allowed("shm", own)
		e.allowed("shm", "/"+own)
		e.denied("shm", other)
		e.allowed("sysvshm", key)
	})
	t.Run("host", func(t *testing.T) {
		s := spec
		s.IPC = IPCHost
		e := newProbeEnv(t, s)
		e.allowed("shm", other)
	})
	t.Run("none", func(t *testing.T) {
		s := spec
		s.IPC = IPCNone
		e := newProbeEnv(t, s)
		e.denied("shm", own)
		e.denied("sysvshm", key)
	})
}
