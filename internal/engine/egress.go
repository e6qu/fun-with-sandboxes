package engine

import (
	"net"
	"strconv"

	"github.com/e6qu/fun-with-sandboxes/internal/egress"
)

const egressKey = "_egress/tcp"

// hasEgress reports whether the container reaches the internet: it has a
// network, and not every one of its networks is internal, as in Docker.
func (e *Engine) hasEgress(c *Container) bool {
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

// ensureEgress starts the stack's egress proxy on a port of its block and
// returns that port.
func (e *Engine) ensureEgress(stack string) (int, error) {
	e.egressMu.Lock()
	defer e.egressMu.Unlock()
	port, err := e.alloc.Port(stack, egressKey)
	if err != nil {
		return 0, err
	}
	if _, ok := e.egress[stack]; ok {
		return port, nil
	}
	p, err := egress.Listen(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), egress.HostPolicy())
	if err != nil {
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

func (e *Engine) closeEgress(stack string) {
	e.egressMu.Lock()
	defer e.egressMu.Unlock()
	if p, ok := e.egress[stack]; ok {
		p.Close()
		delete(e.egress, stack)
	}
}

func (e *Engine) closeAllEgress() {
	e.egressMu.Lock()
	defer e.egressMu.Unlock()
	for stack, p := range e.egress {
		p.Close()
		delete(e.egress, stack)
	}
}
