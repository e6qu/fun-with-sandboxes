// Package alloc hands each stack a disjoint block of host ports and its own
// directory tree, and persists the assignments so they survive restarts.
package alloc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
)

// Compose project names follow this pattern; holding stack names to it also
// keeps them safe to use as a single path component.
var stackName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

var (
	ErrNoBlock     = errors.New("alloc: no free port block")
	ErrBlockFull   = errors.New("alloc: stack port block exhausted")
	ErrInvalidName = errors.New("alloc: invalid stack name")
)

type Config struct {
	// Root holds the state file and one directory per stack.
	Root string
	// PortBase and PortLimit bound the host ports the allocator hands out,
	// PortLimit exclusive.
	PortBase, PortLimit int
	// BlockSize is the number of ports reserved for each stack.
	BlockSize int
}

type Stack struct {
	Name      string         `json:"name"`
	PortBase  int            `json:"portBase"`
	BlockSize int            `json:"blockSize"`
	Ports     map[string]int `json:"ports"`
}

// Contains reports whether port lies inside the stack's block.
func (s *Stack) Contains(port int) bool {
	return port >= s.PortBase && port < s.PortBase+s.BlockSize
}

// AssignedPorts returns the host ports handed out so far, ascending.
func (s *Stack) AssignedPorts() []int {
	out := make([]int, 0, len(s.Ports))
	for _, p := range s.Ports {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

type state struct {
	Stacks map[string]*Stack `json:"stacks"`
}

type Allocator struct {
	cfg Config
	mu  sync.Mutex
	st  state
	// leased holds the ports of each stack's block lent out for a while,
	// kept in memory: a lease ends with the engine that made it.
	leased map[string]map[int]bool
}

func Open(cfg Config) (*Allocator, error) {
	if cfg.BlockSize <= 0 || cfg.PortBase <= 0 || cfg.PortLimit > 65536 || cfg.PortBase+cfg.BlockSize > cfg.PortLimit {
		return nil, fmt.Errorf("alloc: invalid port range %d-%d with block %d", cfg.PortBase, cfg.PortLimit, cfg.BlockSize)
	}
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, err
	}
	a := &Allocator{cfg: cfg, st: state{Stacks: map[string]*Stack{}}}
	b, err := os.ReadFile(a.statePath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &a.st); err != nil {
			return nil, fmt.Errorf("alloc: read %s: %w", a.statePath(), err)
		}
		if a.st.Stacks == nil {
			a.st.Stacks = map[string]*Stack{}
		}
	}
	return a, nil
}

func (a *Allocator) statePath() string { return filepath.Join(a.cfg.Root, "alloc.json") }

// Dir returns the directory that holds everything belonging to the stack.
func (a *Allocator) Dir(stack string) (string, error) {
	if !stackName.MatchString(stack) {
		return "", fmt.Errorf("%w: %q", ErrInvalidName, stack)
	}
	return filepath.Join(a.cfg.Root, "stacks", stack), nil
}

// Stack returns the stack's assignment, reserving a block on first use.
func (a *Allocator) Stack(name string) (Stack, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.stackLocked(name)
	if err != nil {
		return Stack{}, err
	}
	return copyStack(s), nil
}

func (a *Allocator) stackLocked(name string) (*Stack, error) {
	dir, err := a.Dir(name)
	if err != nil {
		return nil, err
	}
	if s, ok := a.st.Stacks[name]; ok {
		return s, nil
	}
	base, err := a.freeBlockLocked()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Stack{Name: name, PortBase: base, BlockSize: a.cfg.BlockSize, Ports: map[string]int{}}
	a.st.Stacks[name] = s
	if err := a.saveLocked(); err != nil {
		delete(a.st.Stacks, name)
		return nil, err
	}
	return s, nil
}

func (a *Allocator) freeBlockLocked() (int, error) {
	used := map[int]bool{}
	for _, s := range a.st.Stacks {
		used[s.PortBase] = true
	}
	for base := a.cfg.PortBase; base+a.cfg.BlockSize <= a.cfg.PortLimit; base += a.cfg.BlockSize {
		if !used[base] {
			return base, nil
		}
	}
	return 0, ErrNoBlock
}

// Port returns the host port assigned to key (for example "db/5432") in the
// stack, assigning the lowest free port of the block on first use. A port is
// free when nothing else in the stack holds it and the host can bind it.
func (a *Allocator) Port(stack, key string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.stackLocked(stack)
	if err != nil {
		return 0, err
	}
	if p, ok := s.Ports[key]; ok {
		return p, nil
	}
	taken := a.takenLocked(s)
	for p := s.PortBase; p < s.PortBase+s.BlockSize; p++ {
		if taken[p] || !hostPortFree(p) {
			continue
		}
		s.Ports[key] = p
		if err := a.saveLocked(); err != nil {
			delete(s.Ports, key)
			return 0, err
		}
		return p, nil
	}
	return 0, fmt.Errorf("%w: %s", ErrBlockFull, stack)
}

func (a *Allocator) takenLocked(s *Stack) map[int]bool {
	taken := map[int]bool{}
	for _, p := range s.Ports {
		taken[p] = true
	}
	for p := range a.leased[s.Name] {
		taken[p] = true
	}
	return taken
}

// Lease lends out a free port of the stack's block until Unlease returns
// it, for a socket the engine opens on the stack's behalf that the stack's
// containers must be able to reach, such as the source of a UDP flow its
// published port forwards.
func (a *Allocator) Lease(stack string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.st.Stacks[stack]
	if !ok {
		return 0, fmt.Errorf("alloc: stack %s holds no block", stack)
	}
	taken := a.takenLocked(s)
	for p := s.PortBase; p < s.PortBase+s.BlockSize; p++ {
		if taken[p] || !hostPortFree(p) {
			continue
		}
		if a.leased == nil {
			a.leased = map[string]map[int]bool{}
		}
		if a.leased[stack] == nil {
			a.leased[stack] = map[int]bool{}
		}
		a.leased[stack][p] = true
		return p, nil
	}
	return 0, fmt.Errorf("%w: %s", ErrBlockFull, stack)
}

// Unlease returns a leased port to the stack's block.
func (a *Allocator) Unlease(stack string, port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.leased[stack], port)
}

// Release frees the stack's block and removes its directory tree.
func (a *Allocator) Release(stack string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	dir, err := a.Dir(stack)
	if err != nil {
		return err
	}
	if _, ok := a.st.Stacks[stack]; !ok {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	delete(a.st.Stacks, stack)
	delete(a.leased, stack)
	return a.saveLocked()
}

// Stacks lists every stack holding a block.
func (a *Allocator) Stacks() []Stack {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Stack, 0, len(a.st.Stacks))
	for _, s := range a.st.Stacks {
		out = append(out, copyStack(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (a *Allocator) saveLocked() error {
	b, err := json.MarshalIndent(a.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.statePath())
}

func copyStack(s *Stack) Stack {
	c := *s
	c.Ports = make(map[string]int, len(s.Ports))
	for k, v := range s.Ports {
		c.Ports[k] = v
	}
	return c
}

// hostPortFree reports whether the host can bind p for both TCP and UDP: a
// container port takes one host port for both protocols.
func hostPortFree(p int) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	u, err := net.ListenPacket("udp", addr)
	if err != nil {
		return false
	}
	u.Close()
	return true
}
