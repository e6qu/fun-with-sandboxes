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

// No default capability grants a container here a power to give up: they
// grant powers over other users' files and processes, devices or identity,
// which a container running as the engine's unprivileged user does not
// hold, and NET_RAW's ICMP reaches nothing, as Seatbelt holds a container's
// traffic to its stack's TCP and UDP ports. Dropping any of them changes
// nothing that is not already so. no-new-privileges holds for every
// container: none can run a program outside its root but the system shells,
// and unpacking strips set-user-ID bits.

func capName(c string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(c)), "CAP_")
}

// checkSecurity accepts capabilities and security options as Docker does,
// refusing what needs root or a Linux kernel.
func checkSecurity(hc *container.HostConfig) error {
	known := map[string]bool{}
	for _, c := range defaultCapabilities {
		known[c] = true
	}
	for _, c := range hc.CapDrop {
		n := capName(c)
		switch {
		case n == "ALL", known[n], isLinuxCapability(n):
		default:
			return invalid("unknown capability to drop: %q", c)
		}
	}
	for _, c := range hc.CapAdd {
		n := capName(c)
		switch {
		case known[n]:
		case n == "ALL" || isLinuxCapability(n):
			return unsupported("capability %s is not supported: it needs root, which this engine runs without", n)
		default:
			return invalid("unknown capability to add: %q", c)
		}
	}
	for _, o := range hc.SecurityOpt {
		k, v, _ := strings.Cut(strings.Replace(o, ":", "=", 1), "=")
		switch {
		case k == "no-new-privileges" && (v == "" || v == "true" || v == "false"):
		case (k == "seccomp" || k == "apparmor") && v == "unconfined", k == "label" && v == "disable":
			// Linux confinement this engine never applies.
		case k == "seccomp" || k == "apparmor" || k == "label":
			return unsupported("security option %q is not supported: Seatbelt confines containers here", o)
		default:
			return invalid("invalid --security-opt: %q", o)
		}
	}
	return nil
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
