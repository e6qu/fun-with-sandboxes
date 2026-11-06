// Package seatbelt renders the Seatbelt (SBPL) profile that confines one
// container: writes only to its root and volumes, no reads of user data, binds
// only on its own ports, connects only to its stack's ports, and no signals or
// process inspection beyond its own sandbox.
package seatbelt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SystemReadPaths are the host trees every macOS process needs to run and
// that carry no user data.
var SystemReadPaths = []string{
	"/System",
	"/usr/lib",
	"/usr/share",
	"/private/var/db/timezone",
	"/private/var/db/dyld",
	"/Library/Apple",
	"/dev",
}

// DeniedReadRoots hold user data or other stacks' state; the container root
// and volumes are re-allowed inside them.
var DeniedReadRoots = []string{
	"/Users",
	"/Volumes",
	"/private/var/folders",
	"/private/var/root",
	"/private/tmp",
	"/Library",
	"/Applications",
	"/opt",
	"/usr/local",
	"/nix",
	"/private/etc/ssh",
}

// SystemShells are the only host executables a container may run: the C
// library's popen() and system() hard-code /bin/sh, which on macOS hands off
// to the shell /private/var/select/sh names, and a script names its
// interpreter as /usr/bin/env finds it on the container's PATH, since the
// kernel resolves a script's interpreter on the host. env runs nothing the
// profile would not let the container run itself.
var SystemShells = []string{"/bin/sh", "/bin/bash", "/bin/zsh", "/bin/dash", "/usr/bin/env"}

// MachServices are the only global Mach services a container may look up:
// user/group lookup, notifications, logging, and certificate trust
// evaluation, which TLS needs and which hands out no secrets. Everything
// else — the keychain, pasteboard, Launch Services, TCC — stays out of reach.
var MachServices = []string{
	"com.apple.system.opendirectoryd.libinfo",
	"com.apple.system.opendirectoryd.membership",
	"com.apple.system.DirectoryService.libinfo_v1",
	"com.apple.system.notification_center",
	"com.apple.system.logger",
	"com.apple.logd",
	"com.apple.diagnosticd",
	"com.apple.trustd",
	"com.apple.trustd.agent",
}

type Volume struct {
	Path     string
	ReadOnly bool
}

type Spec struct {
	// Root is the container's writable root directory.
	Root string
	// StateRoot is the engine's state directory, denied in full except for
	// Root and the volumes.
	StateRoot string
	Volumes   []Volume
	// BindPorts are the loopback ports the container may listen on.
	BindPorts []int
	// ConnectPorts are the loopback ports the container may connect to: its
	// own and its stack peers'.
	ConnectPorts []int
	// ReadOnlyRoot denies writes to Root; volumes and tmpfs mounts, which
	// live outside it, stay as their own entries say.
	ReadOnlyRoot bool
	// IPC confines POSIX shared memory and semaphores, and System V IPC.
	IPC IPCMode
	// IPCPrefix is the name prefix POSIX IPC objects must carry under
	// IPCPrivate.
	IPCPrefix string
	// Libraries are files outside the root every process of the container
	// loads, such as the container library.
	Libraries []string
	// ReadFiles are files outside the root the container reads, such as its
	// stack's network file.
	ReadFiles []string
	// Helper is the engine's helper binary, which the container's sandbox
	// executes once, to start the anchor that runs its execs.
	Helper string
	// PortDir is where the container listens, with Unix sockets, on its TCP
	// ports that have no host port.
	PortDir string
	// HostPIDs lets the container signal and inspect every process the
	// engine's user may, as on the host's PID namespace.
	HostPIDs bool
	// ProcsSocket is where the container's shim answers it about its PID
	// namespace (package pidns).
	ProcsSocket string
}

// IPCMode is a container's IPC confinement.
type IPCMode int

const (
	// IPCPrivate confines POSIX IPC names to a prefix, as a private IPC
	// namespace confines them to a container; System V IPC, which Seatbelt
	// cannot tell apart by key, stays open.
	IPCPrivate IPCMode = iota
	// IPCHost leaves IPC open.
	IPCHost
	// IPCNone denies POSIX and System V IPC.
	IPCNone
)

var errBadPath = errors.New("seatbelt: path must be absolute and free of quotes, backslashes and control characters")

func checkPath(p string) error {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\"\\") {
		return fmt.Errorf("%w: %q", errBadPath, p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %q", errBadPath, p)
		}
	}
	return nil
}

func checkPort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("seatbelt: port %d out of range", p)
	}
	return nil
}

// Render returns the profile text for the spec.
func Render(s Spec) (string, error) {
	paths := []string{s.Root, s.StateRoot}
	if s.PortDir != "" {
		paths = append(paths, s.PortDir)
	}
	for _, v := range s.Volumes {
		paths = append(paths, v.Path)
	}
	extra := append(append([]string{}, s.Libraries...), s.ReadFiles...)
	if s.Helper != "" {
		extra = append(extra, s.Helper)
	}
	if s.ProcsSocket != "" {
		extra = append(extra, s.ProcsSocket)
	}
	for _, l := range extra {
		if err := checkPath(l); err != nil {
			return "", err
		}
	}
	for _, p := range paths {
		if err := checkPath(p); err != nil {
			return "", err
		}
	}
	if s.IPC == IPCPrivate && !ipcPrefixOK(s.IPCPrefix) {
		return "", fmt.Errorf("seatbelt: IPC prefix %q must be letters, digits and dots", s.IPCPrefix)
	}
	for _, p := range append(append([]int{}, s.BindPorts...), s.ConnectPorts...) {
		if err := checkPort(p); err != nil {
			return "", err
		}
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("(version 1)")
	// Deny every operation the profile does not name, on top of Apple's
	// baseline for any process, which dyld and libSystem need to start one.
	w("(deny default)")
	w("(import \"system.sb\")")
	w("(allow process-fork)")
	w("(allow sysctl-read)")
	w("(allow file-ioctl)")
	w("(allow pseudo-tty)")
	w("(allow ipc-posix* ipc-sysv*)")
	w("(allow network-inbound)")
	w("(allow system-socket)")
	// Kernel control and event sockets, and raw link-layer ones.
	w("(deny system-socket (socket-domain 32) (socket-domain 27))")

	// SBPL applies the last matching rule, so each section denies broadly and
	// then re-allows what the container owns.
	w("(deny file-write*)")
	w("(allow file-write*")
	if !s.ReadOnlyRoot {
		w("  (subpath %q)", s.Root)
	}
	for _, v := range s.Volumes {
		if !v.ReadOnly {
			w("  (subpath %q)", v.Path)
		}
	}
	if s.PortDir != "" {
		w("  (subpath %q)", s.PortDir)
	}
	for _, d := range []string{"/dev/null", "/dev/zero", "/dev/dtracehelper", "/dev/tty"} {
		w("  (literal %q)", d)
	}
	w("  (regex #\"^/dev/fd/\")")
	w("  (regex #\"^/dev/ttys[0-9]+$\"))")

	// A rule without a filter outranks every filtered rule of its
	// operation, wherever it stands, so the broad allowance names a subpath.
	w("(allow file-read* (subpath \"/\"))")
	w("(deny file-read*")
	for _, d := range append(append([]string{}, DeniedReadRoots...), s.StateRoot) {
		w("  (subpath %q)", d)
	}
	w(")")
	w("(allow file-read*")
	w("  (subpath %q)", s.Root)
	if s.PortDir != "" {
		w("  (subpath %q)", s.PortDir)
	}
	for _, v := range s.Volumes {
		w("  (subpath %q)", v.Path)
	}
	for _, d := range SystemReadPaths {
		w("  (subpath %q)", d)
	}
	for _, l := range s.Libraries {
		w("  (literal %q)", l)
	}
	for _, f := range s.ReadFiles {
		w("  (literal %q)", f)
	}
	if s.Helper != "" {
		w("  (literal %q)", s.Helper)
	}
	w(")")
	if len(s.Libraries) > 0 {
		w("(allow file-map-executable")
		for _, l := range s.Libraries {
			w("  (literal %q)", l)
		}
		w(")")
	}
	// stat and realpath walk each ancestor of the root and volumes; they may
	// learn the ancestors exist, but not list them.
	w("(allow file-read-metadata")
	w("  (literal \"/\")")
	seen := map[string]bool{}
	for _, p := range paths {
		for _, a := range ancestors(p) {
			if !seen[a] {
				seen[a] = true
				w("  (literal %q)", a)
			}
		}
	}
	w(")")

	// A container executes its root's programs and its mounts', as Docker
	// lets it, and a script's interpreter, which Seatbelt checks apart.
	w("(deny process-exec*)")
	w("(allow process-exec* process-exec-interpreter")
	w("  (subpath %q)", s.Root)
	for _, v := range s.Volumes {
		w("  (subpath %q)", v.Path)
	}
	for _, sh := range SystemShells {
		w("  (literal %q)", sh)
	}
	if s.Helper != "" {
		w("  (literal %q)", s.Helper)
	}
	w(")")

	w("(deny network-bind)")
	w("(allow network-bind (local unix-socket (subpath %q)))", s.Root)
	// A container makes and reaches Unix sockets in its mounts as in its
	// root, as Docker allows: a bind-mounted socket, such as an engine's, is
	// one the user handed it.
	for _, v := range s.Volumes {
		if !v.ReadOnly {
			w("(allow network-bind (local unix-socket (subpath %q)))", v.Path)
		}
	}
	for _, p := range uniq(s.BindPorts) {
		w("(allow network-bind (local ip \"localhost:%d\"))", p)
	}
	if s.PortDir != "" {
		w("(allow network-bind (local unix-socket (subpath %q)))", s.PortDir)
	}
	w("(deny network-outbound)")
	w("(allow network-outbound (remote unix-socket (subpath %q)))", s.Root)
	if s.PortDir != "" {
		w("(allow network-outbound (remote unix-socket (subpath %q)))", s.PortDir)
	}
	for _, v := range s.Volumes {
		w("(allow network-outbound (remote unix-socket (subpath %q)))", v.Path)
	}
	if s.ProcsSocket != "" {
		w("(allow network-outbound (remote unix-socket (path-literal %q)))", s.ProcsSocket)
	}
	w("(allow network-outbound (remote unix-socket (path-literal \"/private/var/run/syslog\")))")
	for _, p := range uniq(s.ConnectPorts) {
		w("(allow network-outbound (remote ip \"localhost:%d\"))", p)
	}

	w("(deny mach-lookup)")
	w("(allow mach-lookup")
	for _, m := range MachServices {
		w("  (global-name %q)", m)
	}
	if s.IPC == IPCPrivate {
		// The container's own Mach services, which the container library
		// registers under its IPC prefix, as POSIX IPC names.
		w("  (global-name-prefix %q)", s.IPCPrefix)
	}
	w(")")
	if s.IPC == IPCPrivate {
		w("(allow mach-register (global-name-prefix %q))", s.IPCPrefix)
	}

	switch s.IPC {
	case IPCPrivate:
		w("(deny ipc-posix-shm* ipc-posix-sem*)")
		// Programs name POSIX IPC objects with or without a leading slash.
		w("(allow ipc-posix-shm* ipc-posix-sem* (ipc-posix-name-prefix %q) (ipc-posix-name-prefix %q))", s.IPCPrefix, "/"+s.IPCPrefix)
	case IPCNone:
		w("(deny ipc-posix-shm* ipc-posix-sem* ipc-sysv*)")
	}

	if s.HostPIDs {
		w("(allow signal)")
		w("(allow process-info*)")
	} else {
		w("(deny signal)")
		w("(allow signal (target same-sandbox))")
		w("(deny process-info*)")
		w("(allow process-info* (target same-sandbox))")
	}

	return b.String(), nil
}

// ancestors returns the directories strictly between / and p, outermost first.
func ancestors(p string) []string {
	var out []string
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i := 1; i < len(parts); i++ {
		out = append(out, "/"+strings.Join(parts[:i], "/"))
	}
	return out
}

func uniq(ps []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range ps {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out
}

func ipcPrefixOK(p string) bool {
	if p == "" {
		return false
	}
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.') {
			return false
		}
	}
	return true
}

// IPCPrefixFor is the prefix of the POSIX shared memory and semaphore names
// the sandbox keyed by key may use: short, as macOS caps those names at 31
// bytes.
func IPCPrefixFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "fws" + hex.EncodeToString(sum[:4]) + "."
}
