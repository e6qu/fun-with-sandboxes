//go:build e2e && darwin

package e2e

import (
	"fmt"
	"math/rand/v2"
	"net"
	"sync/atomic"
	"testing"
)

// freePorts returns the first of n consecutive loopback ports nothing
// listens on, TCP or UDP, for a container or a client to bind itself. They
// come from below the ephemeral range (49152 up on macOS), which the
// kernel hands out to the engine's and every other process's sockets: a
// port from that range could be taken again before it is bound.
func freePorts(t *testing.T, n int) int {
	t.Helper()
	for range 1000 {
		lo := int(nextPort.Add(int64(n)))%(49152-20000-n) + 20000
		if portsFree(lo, n) {
			return lo
		}
	}
	t.Fatalf("no %d free ports", n)
	return 0
}

func freePort(t *testing.T) int {
	t.Helper()
	return freePorts(t, 1)
}

func portsFree(lo, n int) bool {
	var held []interface{ Close() error }
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for p := lo; p < lo+n; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			return false
		}
		held = append(held, l)
		u, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			return false
		}
		held = append(held, u)
	}
	return true
}

var nextPort = func() *atomic.Int64 {
	var n atomic.Int64
	n.Store(int64(rand.IntN(49152 - 20000)))
	return &n
}()
