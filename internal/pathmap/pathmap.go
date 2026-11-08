// Package pathmap builds fwsroot, the library the engine loads into every
// container process to give it the container's view of paths, identity,
// users and groups, and hostname (see src/fwsroot.c). The engine compiles it
// from the source it embeds, with the host's C compiler, once per version.
package pathmap

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

//go:embed src/fwsroot.c
var source []byte

// HostnameVar names the hostname the library presents; package identity
// names the user and group.
const HostnameVar = "FWS_HOSTNAME"

// DomainnameVar names the NIS domain name the library presents with the
// hostname.
const DomainnameVar = "FWS_DOMAINNAME"

// PortDirVar names the directory where the library listens, with Unix
// sockets, on the container's TCP ports that have no host port; RouterVar
// names the port of the container's router (package router), through which
// its own processes reach them.
const (
	PortDirVar = "FWS_PORTDIR"
	RouterVar  = "FWS_ROUTER"
)

// PidsVar names the file of the container's pids limit, which the engine
// rewrites as the limit changes: the library fails a fork with EAGAIN once
// the container runs as many processes.
const PidsVar = "FWS_PIDS"

// PidNSVar is "host" for a container on the host's PID namespace: the
// library presents no pid 1 and lists every process the host does. For a
// container on another's PID namespace it is the host pid of that
// namespace's pid 1.
const PidNSVar = "FWS_PIDNS"

// PidNSFileVar names the file of the container's PID namespace, and
// ProcsVar the socket its shim answers on about it (package pidns).
const (
	PidNSFileVar = "FWS_PIDNSFILE"
	ProcsVar     = "FWS_PROCS"
)

// HelperVar names the engine's helper binary, as a process listing shows
// it: the library leaves the container's exec anchor out of listings.
const HelperVar = "FWS_HELPER"

// NetworkVar names the stack's network file, which lists its members'
// addresses, names and host ports; AddressVar names the container's own
// member address.
const (
	NetworkVar = "FWS_NETWORK"
	AddressVar = "FWS_ADDRESS"
)

// Mount is a volume or bind mount: Dest in the container, and Source, the
// host directory or file behind it.
type Mount struct{ Dest, Source string }

// MountsVar is the variable that presents mounts at their destinations, or
// "" when there are none. Each source is resolved, as the kernel reports a
// working directory inside it.
func MountsVar(ms []Mount) (string, error) {
	var lines []string
	for _, m := range ms {
		src, err := filepath.EvalSymlinks(m.Source)
		if err != nil {
			return "", err
		}
		if strings.ContainsAny(m.Dest+src, "\t\n") {
			return "", fmt.Errorf("mount %s at %s: a tab or newline in a path", src, m.Dest)
		}
		lines = append(lines, filepath.Clean(m.Dest)+"\t"+src)
	}
	if len(lines) == 0 {
		return "", nil
	}
	return "FWS_MOUNTS=" + strings.Join(lines, "\n"), nil
}

// PortsVar is the variable that maps the container's ports, keyed as
// "80/tcp", onto the host ports behind them, or "" when there are none: the
// library binds and reaches a container port through its host port.
func PortsVar(ports map[string]int) (string, error) {
	keys := make([]string, 0, len(ports))
	for k := range ports {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var entries []string
	for _, k := range keys {
		num, proto, _ := strings.Cut(k, "/")
		if proto != "tcp" && proto != "udp" {
			continue
		}
		if _, err := strconv.ParseUint(num, 10, 16); err != nil {
			return "", fmt.Errorf("port %q: %w", k, err)
		}
		entries = append(entries, fmt.Sprintf("%s/%s=%d", num, proto, ports[k]))
	}
	if len(entries) == 0 {
		return "", nil
	}
	return "FWS_PORTS=" + strings.Join(entries, ","), nil
}

// archs are the architectures the library is built for: Apple's own
// programs on Apple silicon are arm64e.
func archs() []string {
	if runtime.GOARCH == "arm64" {
		return []string{"-arch", "arm64", "-arch", "arm64e"}
	}
	return []string{"-arch", "x86_64"}
}

// Build compiles the library into dir unless a build of this source is
// there already, and returns its path.
func Build(dir string) (string, error) {
	sum := sha256.Sum256(source)
	out := filepath.Join(dir, "fwsroot-"+hex.EncodeToString(sum[:8])+".dylib")
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(dir, "build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "fwsroot.c")
	if err := os.WriteFile(src, source, 0o644); err != nil {
		return "", err
	}
	lib := filepath.Join(work, "fwsroot.dylib")
	// The library binds the IOKit functions it stands in for weakly and
	// from whichever image provides them: it links no framework. It binds
	// the socket calls' cancellation-free variants weakly too, as no header
	// declares them.
	args := append([]string{"clang", "-dynamiclib", "-O2", "-Wall", "-Werror",
		"-Wl,-U,_IORegisterForSystemPower", "-Wl,-U,_IONotificationPortCreate",
		"-Wl,-U,_connect$NOCANCEL", "-Wl,-U,_sendto$NOCANCEL", "-Wl,-U,_sendmsg$NOCANCEL",
		"-Wl,-U,_recvfrom$NOCANCEL", "-Wl,-U,_recvmsg$NOCANCEL", "-Wl,-U,_accept$NOCANCEL"}, archs()...)
	cmd := exec.Command("/usr/bin/xcrun", append(args, "-o", lib, src)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build the container library with clang, which the Xcode Command Line Tools provide (xcode-select --install): %v\n%s", err, b)
	}
	// Another engine may have built it meanwhile; either build is the same.
	if err := os.Rename(lib, out); err != nil {
		return "", err
	}
	return out, nil
}
