// Package shim is a container's supervisor: a process of the engine's own
// binary that starts the container's main process, owns its output, its
// terminal and its published ports, enforces its resource limits, and
// outlives the engine, which talks to it over a Unix socket. With a
// supervisor per container the engine can restart, or stop for an upgrade,
// while its containers keep running.
package shim

import (
	"net/netip"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/netstat"
	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
)

// Role is the value of runtime.RoleVar that starts the binary as a shim.
const Role = "shim"

// Config is everything the shim needs to run one container, written by the
// engine to the container's directory.
type Config struct {
	ID string
	// Spec is the main process; its streams are the shim's to make.
	Spec      runtime.Spec
	TTY       bool
	Console   [2]uint `json:",omitempty"`
	OpenStdin bool
	LogPath   string
	Socket    string
	// Anchor is where the container's exec anchor listens.
	Anchor string
	// Router is the loopback port of the container's router, which joins
	// connections to the listeners in PortDir (package router); 0 for none.
	Router  int
	PortDir string
	// NetworkFile and Address locate the container among its stack's
	// members, for its router's relays to other stacks' members.
	NetworkFile string
	Address     string
	ExitPath    string
	// Tag is the environment entry every process of the container starts
	// with.
	Tag string
	// Procs is where the shim answers its container's library about the
	// PID namespace whose file is PidNamespace (package pidns).
	Procs        string    `json:",omitempty"`
	PidNamespace string    `json:",omitempty"`
	Publish      []Binding `json:",omitempty"`
	// AllocRoot and Stack locate the stack's port block, from which UDP
	// flows send.
	AllocRoot string
	Stack     string
	Resources Resources
}

// Binding is a published port: the first host port from Lo to Hi the
// host lets the shim bind on IP, forwarded to Upstream.
type Binding struct {
	Proto    string
	IP       netip.Addr
	Lo, Hi   int
	Upstream string
}

// Resources are the limits the shim enforces while the container runs.
type Resources struct {
	// Memory caps the sum of the processes' physical footprints, in bytes.
	Memory int64 `json:",omitempty"`
	// NanoCPUs caps CPU time at NanoCPUs/1e9 CPUs.
	NanoCPUs int64 `json:",omitempty"`
	// PidsLimit caps the number of processes.
	PidsLimit int64 `json:",omitempty"`
}

func (r Resources) any() bool { return r.Memory > 0 || r.NanoCPUs > 0 || r.PidsLimit > 0 }

// Ready is what the shim reports on descriptor 3 once the container runs,
// or why it does not.
type Ready struct {
	Pid       int    `json:",omitempty"`
	Ports     []int  `json:",omitempty"`
	Error     string `json:",omitempty"`
	PortInUse bool   `json:",omitempty"`
}

// Exit is how the container's main process ended, written to ExitPath.
type Exit struct {
	ExitCode   int
	OOMKilled  bool
	FinishedAt time.Time
}

// Request is the first line of every connection to the shim's socket.
type Request struct {
	Op string
	// Signal is for "signal".
	Signal int `json:",omitempty"`
	// Height and Width are for "resize".
	Height, Width uint16 `json:",omitempty"`
	// Close, for "stdin", closes the container's input once the
	// connection's input ends.
	Close bool `json:",omitempty"`
	// Resources and Nice are for "update".
	Resources *Resources `json:",omitempty"`
	Nice      *int       `json:",omitempty"`
}

// Response is the shim's answer to a request.
type Response struct {
	Error string `json:",omitempty"`
	// State answers "state".
	State *State `json:",omitempty"`
	// Exit answers "wait".
	Exit *Exit `json:",omitempty"`
}

// State is the running container as the shim sees it.
type State struct {
	Pid int
	// Shim is the supervisor's own pid.
	Shim int
	// Members are the processes the shim counts as the container's, and
	// Footprint the sum of their physical footprints, the measure the
	// memory limit holds.
	Members   []int
	Footprint uint64
	// DiskRead and DiskWritten count the bytes the container's processes
	// read from and wrote to disk, those that ended included.
	DiskRead, DiskWritten uint64
	// Net counts the packets and bytes its processes' sockets, and its
	// router's, received and sent, those closed included.
	Net              netstat.Counts
	Periods          uint64
	ThrottledPeriods uint64
	ThrottledTime    time.Duration
	OOMKills         uint64
	Resources        Resources
}
