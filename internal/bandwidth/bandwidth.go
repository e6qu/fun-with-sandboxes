// Package bandwidth holds a container's traffic to a rate, as the
// Kubernetes bandwidth plugin holds a pod's to its
// kubernetes.io/egress-bandwidth and kubernetes.io/ingress-bandwidth
// annotations: a token bucket per direction, which every stream of the
// container shares.
package bandwidth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// The labels, and pod annotations, that set a container's rates.
const (
	EgressLabel  = "kubernetes.io/egress-bandwidth"
	IngressLabel = "kubernetes.io/ingress-bandwidth"
)

// minRate is the lowest rate the Kubernetes bandwidth plugin takes, in
// bits a second.
const minRate = 1000

// suffixes are a quantity's, as Kubernetes reads them.
var suffixes = []struct {
	s string
	m float64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
}

// ParseRate reads a rate in bits a second, a Kubernetes quantity such as
// "10M" or "512Ki".
func ParseRate(s string) (int64, error) {
	num, mult := s, 1.0
	for _, x := range suffixes {
		if n, ok := strings.CutSuffix(s, x.s); ok {
			num, mult = n, x.m
			break
		}
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("bandwidth %q is not a quantity of bits a second, such as 10M", s)
	}
	bps := int64(f * mult)
	if bps < minRate {
		return 0, fmt.Errorf("bandwidth %q is below 1k bits a second", s)
	}
	return bps, nil
}

// chunk is the most a copy writes at once, and the bucket's burst.
const chunk = 32 << 10

// NewLimiter is a bucket for bps bits a second, nil for none.
func NewLimiter(bps int64) *rate.Limiter {
	if bps <= 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(float64(bps)/8), chunk)
}

// Copy copies src to dst as io.Copy does, at l's rate when l is set.
func Copy(dst io.Writer, src io.Reader, l *rate.Limiter) (int64, error) {
	if l == nil {
		return io.Copy(dst, src)
	}
	buf := make([]byte, chunk)
	var n int64
	for {
		r, err := src.Read(buf)
		if r > 0 {
			if werr := l.WaitN(context.Background(), r); werr != nil {
				return n, werr
			}
			w, werr := dst.Write(buf[:r])
			n += int64(w)
			if werr != nil {
				return n, werr
			}
		}
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// Rates are a container's rates in bits a second; 0 is no limit.
type Rates struct{ Egress, Ingress int64 }

// RatesPath is where the engine keeps the rates of a stack's members, by
// member address, beside the stack's network file, for its egress proxy.
func RatesPath(networkFile string) string { return networkFile + ".rates" }

// Conn holds what is written to c to out's rate, and what is read from it
// to in's; a nil limiter is no limit.
func Conn(c net.Conn, out, in *rate.Limiter) net.Conn {
	if out == nil && in == nil {
		return c
	}
	return &limitedConn{Conn: c, out: out, in: in}
}

type limitedConn struct {
	net.Conn
	out, in *rate.Limiter
}

func (c *limitedConn) Read(b []byte) (int, error) {
	if c.in != nil && len(b) > chunk {
		b = b[:chunk]
	}
	n, err := c.Conn.Read(b)
	if n > 0 && c.in != nil {
		if werr := c.in.WaitN(context.Background(), n); werr != nil && err == nil {
			err = werr
		}
	}
	return n, err
}

func (c *limitedConn) Write(b []byte) (int, error) {
	if c.out == nil {
		return c.Conn.Write(b)
	}
	var n int
	for len(b) > 0 {
		k := min(len(b), chunk)
		if err := c.out.WaitN(context.Background(), k); err != nil {
			return n, err
		}
		w, err := c.Conn.Write(b[:k])
		n += w
		if err != nil {
			return n, err
		}
		b = b[k:]
	}
	return n, nil
}

// CloseWrite half-closes c, as a TCP connection's CloseWrite does.
func (c *limitedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Table holds the limiters of a stack's members, by member address, as the
// rates file at path names them, read again when it changes: every
// stream of a member shares its pair.
type Table struct {
	path   string
	mu     sync.Mutex
	mod    time.Time
	size   int64
	byAddr map[string]*pair
}

type pair struct {
	rates   Rates
	out, in *rate.Limiter
}

// NewTable reads the rates at path, which may not exist.
func NewTable(path string) *Table { return &Table{path: path, byAddr: map[string]*pair{}} }

// Limiters returns the member at address's limiters, what it sends and
// what it receives; nil for no limit.
func (t *Table) Limiters(address string) (out, in *rate.Limiter) {
	if t == nil || address == "" {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reload()
	if p, ok := t.byAddr[address]; ok {
		return p.out, p.in
	}
	return nil, nil
}

func (t *Table) reload() {
	info, err := os.Stat(t.path)
	if err != nil {
		t.byAddr, t.mod, t.size = map[string]*pair{}, time.Time{}, 0
		return
	}
	if info.ModTime().Equal(t.mod) && info.Size() == t.size {
		return
	}
	b, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var rates map[string]Rates
	if json.Unmarshal(b, &rates) != nil {
		return
	}
	next := map[string]*pair{}
	for a, r := range rates {
		// A member whose rates stay keeps its buckets.
		if p, ok := t.byAddr[a]; ok && p.rates == r {
			next[a] = p
			continue
		}
		next[a] = &pair{rates: r, out: NewLimiter(r.Egress), in: NewLimiter(r.Ingress)}
	}
	t.byAddr, t.mod, t.size = next, info.ModTime(), info.Size()
}
