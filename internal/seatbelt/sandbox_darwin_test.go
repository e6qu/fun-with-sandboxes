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
	})
}
