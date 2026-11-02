package engine

import (
	"strings"

	"github.com/moby/moby/api/types/container"

	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
)

// defaultCapabilities are the capabilities Docker grants a container.
var defaultCapabilities = []string{
	"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD", "NET_BIND_SERVICE",
	"NET_RAW", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT",
}

// heldCapabilities are the default capabilities a container here exercises
// a real power through: NET_RAW, the ICMP datagram sockets an unprivileged
// macOS process may open. The others grant powers over other users' files
// and processes, devices, or identity, none of which a container running as
// the engine's own unprivileged user holds, so dropping them changes
// nothing that is not already so.
var heldCapabilities = map[string]bool{"NET_RAW": true}

// security is what a container's capabilities and security options allow.
type security struct {
	// ICMP is whether the container may open ICMP datagram sockets.
	ICMP bool
	// NoNewPrivileges records the option; it holds for every container, as
	// none can run a program outside its root but the system shells, and
	// unpacking strips set-user-ID bits.
	NoNewPrivileges bool
}

func capName(c string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(c)), "CAP_")
}

// containerSecurity resolves capabilities and security options as Docker
// does, from the default set, refusing what needs root or a Linux kernel.
func containerSecurity(hc *container.HostConfig) (security, error) {
	set := map[string]bool{}
	for _, c := range defaultCapabilities {
		set[c] = true
	}
	known := map[string]bool{}
	for _, c := range defaultCapabilities {
		known[c] = true
	}
	for _, c := range hc.CapDrop {
		n := capName(c)
		switch {
		case n == "ALL":
			set = map[string]bool{}
		case known[n] || isLinuxCapability(n):
			delete(set, n)
		default:
			return security{}, invalid("unknown capability to drop: %q", c)
		}
	}
	for _, c := range hc.CapAdd {
		n := capName(c)
		switch {
		case known[n]:
			set[n] = true
		case n == "ALL" || isLinuxCapability(n):
			return security{}, unsupported("capability %s is not supported: it needs root, which this engine runs without", n)
		default:
			return security{}, invalid("unknown capability to add: %q", c)
		}
	}
	s := security{ICMP: set["NET_RAW"]}
	for _, o := range hc.SecurityOpt {
		k, v, _ := strings.Cut(strings.Replace(o, ":", "=", 1), "=")
		switch {
		case k == "no-new-privileges" && (v == "" || v == "true"):
			s.NoNewPrivileges = true
		case k == "no-new-privileges" && v == "false":
		case (k == "seccomp" || k == "apparmor") && v == "unconfined", k == "label" && v == "disable":
			// Linux confinement this engine never applies.
		case k == "seccomp" || k == "apparmor" || k == "label":
			return security{}, unsupported("security option %q is not supported: Seatbelt confines containers here", o)
		default:
			return security{}, invalid("invalid --security-opt: %q", o)
		}
	}
	return s, nil
}

// linuxCapabilities are capability names beyond Docker's default set.
var linuxCapabilities = map[string]bool{
	"AUDIT_CONTROL": true, "AUDIT_READ": true, "BLOCK_SUSPEND": true, "BPF": true, "CHECKPOINT_RESTORE": true,
	"DAC_READ_SEARCH": true, "IPC_LOCK": true, "IPC_OWNER": true, "LEASE": true, "LINUX_IMMUTABLE": true,
	"MAC_ADMIN": true, "MAC_OVERRIDE": true, "NET_ADMIN": true, "NET_BROADCAST": true, "PERFMON": true,
	"SYS_ADMIN": true, "SYS_BOOT": true, "SYS_MODULE": true, "SYS_NICE": true, "SYS_PACCT": true,
	"SYS_PTRACE": true, "SYS_RAWIO": true, "SYS_RESOURCE": true, "SYS_TIME": true, "SYS_TTY_CONFIG": true,
	"SYSLOG": true, "WAKE_ALARM": true,
}

func isLinuxCapability(n string) bool { return linuxCapabilities[n] }

// ipcMode resolves --ipc: private (the default, and shareable) confines
// POSIX IPC names to the container's prefix, container:<id> to that
// container's, host leaves them open, and none denies IPC.
func (e *Engine) ipcMode(c *Container) (seatbelt.IPCMode, string, error) {
	mode := string(c.rec.HostConfig.IpcMode)
	switch {
	case mode == "" || mode == "private" || mode == "shareable":
		return seatbelt.IPCPrivate, seatbelt.IPCPrefixFor(c.rec.ID), nil
	case mode == "host":
		return seatbelt.IPCHost, "", nil
	case mode == "none":
		return seatbelt.IPCNone, "", nil
	case strings.HasPrefix(mode, "container:"):
		other, err := e.Container(strings.TrimPrefix(mode, "container:"))
		if err != nil {
			return 0, "", err
		}
		return seatbelt.IPCPrivate, seatbelt.IPCPrefixFor(other.rec.ID), nil
	}
	return 0, "", invalid("invalid IPC mode: %q", mode)
}
