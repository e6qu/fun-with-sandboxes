// Package seatbelt renders the Seatbelt (SBPL) profile that confines one
// container: writes only to its root and volumes, no reads of user data, binds
// only on its own ports, connects only to its stack's ports, and no signals or
// process inspection beyond its own sandbox.
package seatbelt

import (
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
	"/private/etc/ssh",
}

// MachServices are the only global Mach services a container may look up:
// user/group lookup, notifications, and logging. Everything else — keychain,
// pasteboard, Launch Services, TCC — stays out of reach.
var MachServices = []string{
	"com.apple.system.opendirectoryd.libinfo",
	"com.apple.system.opendirectoryd.membership",
	"com.apple.system.DirectoryService.libinfo_v1",
	"com.apple.system.notification_center",
	"com.apple.system.logger",
	"com.apple.logd",
	"com.apple.diagnosticd",
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
}

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
	for _, v := range s.Volumes {
		paths = append(paths, v.Path)
	}
	for _, p := range paths {
		if err := checkPath(p); err != nil {
			return "", err
		}
	}
	for _, p := range append(append([]int{}, s.BindPorts...), s.ConnectPorts...) {
		if err := checkPort(p); err != nil {
			return "", err
		}
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("(version 1)")
	w("(allow default)")

	// SBPL applies the last matching rule, so each section denies broadly and
	// then re-allows what the container owns.
	w("(deny file-write*)")
	w("(allow file-write*")
	w("  (subpath %q)", s.Root)
	for _, v := range s.Volumes {
		if !v.ReadOnly {
			w("  (subpath %q)", v.Path)
		}
	}
	for _, d := range []string{"/dev/null", "/dev/zero", "/dev/dtracehelper", "/dev/tty"} {
		w("  (literal %q)", d)
	}
	w("  (regex #\"^/dev/fd/\")")
	w("  (regex #\"^/dev/ttys[0-9]+$\"))")

	w("(deny file-read*")
	for _, d := range append(append([]string{}, DeniedReadRoots...), s.StateRoot) {
		w("  (subpath %q)", d)
	}
	w(")")
	w("(allow file-read*")
	w("  (subpath %q)", s.Root)
	for _, v := range s.Volumes {
		w("  (subpath %q)", v.Path)
	}
	for _, d := range SystemReadPaths {
		w("  (subpath %q)", d)
	}
	// Path resolution stats each ancestor of the root.
	w("  (literal \"/\")")
	for _, a := range ancestors(s.Root) {
		w("  (literal %q)", a)
	}
	w(")")

	w("(deny process-exec*)")
	w("(allow process-exec* (subpath %q))", s.Root)

	w("(deny network-bind)")
	w("(allow network-bind (local unix-socket (subpath %q)))", s.Root)
	for _, p := range uniq(s.BindPorts) {
		w("(allow network-bind (local ip \"localhost:%d\"))", p)
	}
	w("(deny network-outbound)")
	w("(allow network-outbound (remote unix-socket (subpath %q)))", s.Root)
	w("(allow network-outbound (remote unix-socket (path-literal \"/private/var/run/syslog\")))")
	for _, p := range uniq(s.ConnectPorts) {
		w("(allow network-outbound (remote ip \"localhost:%d\"))", p)
	}

	w("(deny mach-lookup)")
	w("(allow mach-lookup")
	for _, m := range MachServices {
		w("  (global-name %q)", m)
	}
	w(")")

	w("(deny signal)")
	w("(allow signal (target same-sandbox))")
	w("(deny process-info*)")
	w("(allow process-info* (target same-sandbox))")

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
