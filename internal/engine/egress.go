package engine

import (
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/egress"
	"github.com/e6qu/fun-with-sandboxes/internal/procs"
)

const egressKey = "_egress/tcp"

// hasEgress reports whether the container reaches the internet: it has a
// network, and not every one of its networks is internal, as in Docker.
func (e *Engine) hasEgress(c *Container) bool {
	// A container on another's network reaches the internet as that one does.
	c, err := e.netOwner(c)
	if err != nil {
		return false
	}
	if !e.cfg.Egress || c.rec.HostConfig.NetworkMode == "none" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for name := range c.rec.Networks {
		if n, err := e.networkLocked(name); err == nil && !n.Internal && n.Name != "none" {
			return true
		}
	}
	return false
}

// egressProc is a stack's egress proxy process. The proxy runs apart from
// the engine so that, with live restore, it serves the stack's containers
// while no engine runs; start tells it from a later process with its pid.
type egressProc struct {
	Pid   int   `json:"pid"`
	Start int64 `json:"start"`
	Port  int   `json:"port"`
	// Members is the network file the proxy reaches the stack's members by.
	Members string `json:"members"`
	// Key is the file of the key that signs the members' credentials.
	Key string `json:"key"`
}

func (e *Engine) egressFile(stack string) string {
	return filepath.Join(e.cfg.Root, "run", "egress-"+stack+".json")
}

// egressKeyFile holds the key the stack's proxy signs its members'
// credentials with, in the engine's state, which no container reads.
func (e *Engine) egressKeyFile(stack string) string {
	return filepath.Join(e.cfg.Root, "run", "egress-"+stack+".key")
}

// egressCredentials are the proxy credentials of the member at address.
func (e *Engine) egressCredentials(stack, address string) (string, string, error) {
	if address == "" {
		return "", "", nil
	}
	key, err := os.ReadFile(e.egressKeyFile(stack))
	if err != nil {
		return "", "", err
	}
	return address, egress.Password(key, address), nil
}

func processStart(pid int) (int64, bool) {
	es, err := procs.Own()
	if err != nil {
		return 0, false
	}
	for _, p := range es {
		if p.PID == pid {
			return p.Start, true
		}
	}
	return 0, false
}

func (p *egressProc) alive() bool {
	start, ok := processStart(p.Pid)
	return ok && start == p.Start
}

// ensureEgress starts the stack's egress proxy on a port of its block, or
// keeps the one an earlier engine left running, and returns that port.
func (e *Engine) ensureEgress(stack string) (int, error) {
	e.egressMu.Lock()
	defer e.egressMu.Unlock()
	port, err := e.alloc.Port(stack, egressKey)
	if err != nil {
		return 0, err
	}
	nf, err := e.networkFile(stack)
	if err != nil {
		return 0, err
	}
	kf := e.egressKeyFile(stack)
	if p, ok := e.egress[stack]; ok && p.Port == port && p.Members == nf && p.Key == kf && p.alive() {
		return port, nil
	}
	// A proxy an earlier engine left running keeps serving unless it
	// predates the network file or the key, which it would not know.
	var left egressProc
	if ok, _ := readJSON(e.egressFile(stack), &left); ok && left.Port == port && left.alive() {
		if left.Members == nf && left.Key == kf {
			e.egress[stack] = &left
			return port, nil
		}
		stopProcess(&left)
	}
	logf, err := os.OpenFile(filepath.Join(e.cfg.Root, "run", "egress-"+stack+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	// A proxy that replaces one keeps its key, which running members'
	// credentials carry.
	if k, err := os.ReadFile(kf); err != nil || len(k) == 0 {
		key := make([]byte, 32)
		rand.Read(key)
		tmp := kf + ".tmp"
		if err := os.WriteFile(tmp, key, 0o600); err != nil {
			return 0, err
		}
		if err := os.Rename(tmp, kf); err != nil {
			return 0, err
		}
	}
	proc, err := egress.Start(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nf, kf, logf)
	if err != nil {
		return 0, err
	}
	start, _ := processStart(proc.Pid)
	p := &egressProc{Pid: proc.Pid, Start: start, Port: port, Members: nf, Key: kf}
	if err := writeJSON(e.egressFile(stack), p); err != nil {
		stopProcess(p)
		return 0, err
	}
	e.egress[stack] = p
	return port, nil
}

// egressPort returns the stack's proxy port, or 0 if it has none.
func (e *Engine) egressPort(stack string) int {
	s, err := e.alloc.Stack(stack)
	if err != nil {
		return 0
	}
	return s.Ports[egressKey]
}

// BuildEgress returns the egress proxy port BuildKit's build steps use, on a
// stack of their own.
func (e *Engine) BuildEgress() (int, error) {
	if !e.cfg.Egress {
		return 0, nil
	}
	return e.ensureEgress("buildkit")
}

func (e *Engine) closeEgress(stack string) {
	e.egressMu.Lock()
	defer e.egressMu.Unlock()
	e.closeEgressLocked(stack)
}

func (e *Engine) closeEgressLocked(stack string) {
	p, ok := e.egress[stack]
	if !ok {
		var left egressProc
		if found, _ := readJSON(e.egressFile(stack), &left); !found {
			return
		}
		p = &left
	}
	if p.alive() {
		stopProcess(p)
	}
	delete(e.egress, stack)
	os.Remove(e.egressFile(stack))
	os.Remove(e.egressKeyFile(stack))
}

// closeAllEgress stops every stack's proxy, this engine's and any an
// earlier one left running.
func (e *Engine) closeAllEgress() {
	e.egressMu.Lock()
	defer e.egressMu.Unlock()
	files, _ := filepath.Glob(filepath.Join(e.cfg.Root, "run", "egress-*.json"))
	for _, f := range files {
		e.closeEgressLocked(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "egress-"), ".json"))
	}
	for stack := range e.egress {
		e.closeEgressLocked(stack)
	}
}

// stopProcess ends a proxy with SIGTERM, and SIGKILL if it lingers.
func stopProcess(p *egressProc) {
	if !p.alive() {
		return
	}
	syscall.Kill(p.Pid, syscall.SIGTERM)
	if left, err := procs.WaitExit([]int{p.Pid}, egressStopTimeout); err == nil && len(left) == 0 {
		return
	}
	syscall.Kill(p.Pid, syscall.SIGKILL)
	procs.WaitExit([]int{p.Pid}, egressStopTimeout)
}

const egressStopTimeout = 5 * time.Second
