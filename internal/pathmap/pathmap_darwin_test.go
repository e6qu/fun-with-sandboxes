package pathmap

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// probe runs the probe under the library, with the container's root and
// identity in its environment, and returns what each check reported.
func probe(t *testing.T, root string, env []string, checks ...string) map[string]string {
	t.Helper()
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, "bin", "probe"), checks...)
	cmd.Env = append([]string{"DYLD_INSERT_LIBRARIES=" + lib, "SANDBOX_ROOT=" + root}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	return got
}

func containerRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"bin", "etc", "made"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "etc", "marker"), []byte("inside"), 0o644)
	os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\napp:x:1000:50:App:/home/app:/bin/sh\n"), 0o644)
	if out, err := exec.Command("go", "build", "-o", filepath.Join(root, "bin", "probe"), "./testdata/probe").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	return root
}

func TestPathsNameTheRoot(t *testing.T) {
	root := containerRoot(t)
	got := probe(t, root, nil, "read", "system", "write", "list", "cwd", "exec")
	for k, want := range map[string]string{"read": "inside", "system": "ok", "write": "ok", "cwd": "/made", "exec": "read=inside"} {
		if got[k] != want {
			t.Errorf("%s: %q, want %q", k, got[k], want)
		}
	}
	if !strings.Contains(got["list"], "etc,") || !strings.Contains(got["list"], "made,") || strings.Contains(got["list"], "Users,") {
		t.Errorf("/ lists %q, want the container's root", got["list"])
	}
	if _, err := os.Stat(filepath.Join(root, "made", "new")); err != nil {
		t.Errorf("the write did not land in the root: %v", err)
	}
}

// A container's interfaces are its own, as in its network namespace:
// loopback, and on a network, eth0 at its member address, where the host's
// are.
func TestInterfaces(t *testing.T) {
	root := containerRoot(t)
	if got := probe(t, root, []string{AddressVar + "=127.88.0.2"}, "interfaces"); got["interfaces"] != "lo0(loopback 127.0.0.1/8 ),eth0(127.88.0.2/16 02:42:7f:58:00:02)" {
		t.Errorf("on a network: %q", got["interfaces"])
	}
	if got := probe(t, root, nil, "interfaces"); got["interfaces"] != "lo0(loopback 127.0.0.1/8 )" {
		t.Errorf("on no network: %q", got["interfaces"])
	}
}

func TestIdentityAndOwnership(t *testing.T) {
	root := containerRoot(t)
	got := probe(t, root, []string{"FWS_HOSTNAME=web-1"}, "ids", "user", "owner", "chown", "hostname")
	for k, want := range map[string]string{"ids": "0:0", "user": "root:/root", "owner": "0:0", "chown": "1000:50", "hostname": "web-1"} {
		if got[k] != want {
			t.Errorf("%s: %q, want %q", k, got[k], want)
		}
	}
	// The owner persists with the file, for the next process.
	if got := probe(t, root, nil, "owner"); got["owner"] != "1000:50" {
		t.Errorf("owner after chown, in a new process: %q", got["owner"])
	}
	// A user other than root sees itself, and may not give its files away.
	got = probe(t, root, []string{"FWS_UID=1000", "FWS_GID=50"}, "ids", "user", "write")
	if got["ids"] != "1000:50" || got["user"] != "app:/home/app" {
		t.Errorf("as app: ids %q, user %q", got["ids"], got["user"])
	}
	if got := probe(t, root, nil, "owner"); got["owner"] != "1000:50" {
		t.Errorf("owner: %q", got["owner"])
	}
	// root switches to a user for good, with a cleared environment too.
	if got := probe(t, root, nil, "drop"); got["drop"] != "ids=1000:50" {
		t.Errorf("a program executed after setuid runs as %q", got["drop"])
	}
}

func TestMountsAndEnvScripts(t *testing.T) {
	root := containerRoot(t)
	src, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(src, "file"), []byte("mounted"), 0o644)
	// The engine places a mount as a symlink in the root.
	if err := os.Symlink(src, filepath.Join(root, "data")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "bin", "run"), []byte("#!/usr/bin/env -S probe read\n"), 0o755)
	mv, err := MountsVar([]Mount{{Dest: "/data", Source: src}})
	if err != nil {
		t.Fatal(err)
	}
	got := probe(t, root, []string{mv, "PATH=" + filepath.Join(root, "bin")}, "mount", "mountat", "rootpath", "envscript", "empty")
	if want := "mounted,/data/file,/data"; got["mount"] != want {
		t.Errorf("mount: %q, want %q", got["mount"], want)
	}
	if got["mountat"] != "mounted" {
		t.Errorf("mountat: %q, want the mount's file", got["mountat"])
	}
	if want := filepath.Join(src, "file"); got["rootpath"] != want {
		t.Errorf("rootpath: %q, want %q", got["rootpath"], want)
	}
	if !strings.HasPrefix(got["empty"], "error: ") {
		t.Errorf("stat of an empty path: %q, want an error", got["empty"])
	}
	if !strings.Contains(got["envscript"], "read=inside") {
		t.Errorf("a #!/usr/bin/env script ran as %q", got["envscript"])
	}
}

func TestPowerNotificationsWithoutIOKit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "power")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-framework", "IOKit", "-o", bin, "testdata/power/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	run := func(env ...string) string {
		cmd := exec.Command(bin)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("power: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// The sandbox refuses the registration itself.
	if got := run(); got != "0 0" {
		t.Fatalf("without the library: %q, want no registration", got)
	}
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := run("DYLD_INSERT_LIBRARIES=" + lib); got != "1 1" {
		t.Errorf("with the library: %q, want a registration and its run loop source", got)
	}
}

func TestMachServicesUnderTheIPCPrefix(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mach")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-o", bin, "testdata/mach/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	run := func(env ...string) string {
		cmd := exec.Command(bin)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mach: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(); got != "0 0" {
		t.Fatalf("without the library: %q, want the registration refused", got)
	}
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := run("DYLD_INSERT_LIBRARIES="+lib, "SANDBOX_IPC_PREFIX=fwstest."); got != "1 1" {
		t.Errorf("with the library: %q, want the service registered and found under the prefix", got)
	}
}

func TestAbsoluteLinksResolveInTheRoot(t *testing.T) {
	root := containerRoot(t)
	os.MkdirAll(filepath.Join(root, "usr", "local", "bin"), 0o755)
	os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755)
	os.WriteFile(filepath.Join(root, "usr", "local", "bin", "real"), []byte("abs"), 0o644)
	for link, target := range map[string]string{"usr/bin/link": "/usr/local/bin/real", "lib": "/usr/local", "etc/up": "../../.."} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	got := probe(t, root, nil, "abslink")
	if want := "abs,abs,/usr/local/bin/real,/usr/local/bin/real,symlink,abs,inside,inside"; got["abslink"] != want {
		t.Errorf("abslink: %q, want %q", got["abslink"], want)
	}
}

func TestPOSIXIPCUnderTheIPCPrefix(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ipc")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-o", bin, "testdata/ipc/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	run := func(env ...string) string {
		cmd := exec.Command(bin)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ipc: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(); got != "0 0 0" {
		t.Fatalf("without the library: %q, want every name refused", got)
	}
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := run("DYLD_INSERT_LIBRARIES="+lib, "SANDBOX_IPC_PREFIX=fwstest."); got != "1 1 1" {
		t.Errorf("with the library: %q, want shared memory, a semaphore and a long name under the prefix", got)
	}
}

func TestContainerPortsAndDualStack(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "net")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-o", bin, "testdata/net/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	tcp1, tcp2, udp, tcp3, tcp4 := freePort(t, "tcp"), freePort(t, "tcp"), freePort(t, "udp"), freePort(t, "tcp"), freePort(t, "tcp")
	// db listens outside the probe's container, as another member does.
	db, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	go func() {
		for {
			c, err := db.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	peer := db.Addr().(*net.TCPAddr).Port
	ports, err := PortsVar(map[string]int{"8080/tcp": tcp1, "8081/tcp": tcp2, "8082/tcp": tcp3, "8083/tcp": tcp4, "9090/udp": udp})
	if err != nil {
		t.Fatal(err)
	}
	run := func(env ...string) map[string]string {
		cmd := exec.Command(bin, strconv.Itoa(tcp1), strconv.Itoa(tcp2), strconv.Itoa(udp), strconv.Itoa(peer), strconv.Itoa(tcp3), strconv.Itoa(tcp4))
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("net: %v\n%s", err, out)
		}
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			got[k] = v
		}
		return got
	}
	if got := run(); got["listen6"] != "error" {
		t.Fatalf("without the library: %v, want the container port's bind refused", got)
	}
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	network := filepath.Join(t.TempDir(), "network")
	if err := os.WriteFile(network, []byte(fmt.Sprintf("127.88.0.2\tapp\t8080/tcp=%d\t0\tproj_default\t0\n127.88.0.3\tdb proj-db-1\t5432/tcp=%d\t0\tproj_default\t0\t\t127.88.0.9/theirs 127.88.0.2/database\n", tcp1, peer)), 0o644); err != nil {
		t.Fatal(err)
	}
	// A Unix socket's path is capped at 104 bytes, which a socket in a
	// root under the test's temporary directory exceeds. Seatbelt matches
	// the socket's path as given, so the root is resolved, as the engine's.
	root, err := os.MkdirTemp("/tmp", "fwsr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	if err := os.WriteFile(filepath.Join(root, "etc", "hosts"), []byte("127.0.0.1\tlocalhost\n# 10.9.9.9 extra.local\n10.1.2.3\textra.local other  # --add-host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	portDir, err := os.MkdirTemp("/tmp", "fwsp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(portDir) })
	got := run("DYLD_INSERT_LIBRARIES="+lib, "SANDBOX_ROOT="+root, ports, NetworkVar+"="+network, AddressVar+"=127.88.0.2", PortDirVar+"="+portDir, RouterVar+"=1")
	for k, want := range map[string]string{
		"listen6":  "[::]:8080",
		"client4":  "127.0.0.1:8080",
		"accepted": "v6",
		"client6":  "[::ffff:127.0.0.1]:8080",
		"lo6":      "[::1]:8080 own-v6",
		"only6":    "[::1]:8083",
		"listen4":  "0.0.0.0:8081",
		"udp":      "[::]",
		"member":   "127.88.0.3:5432",
		"alias":    "127.88.0.3:5432",
		"hidden":   "unknown",
		"hosts":    "10.1.2.3:80 extra.local",
		"hostent":  "10.1.2.3",
		"direct":   "127.0.0.1:8082",
		"ifaddrs":  "lo0=127.0.0.1,eth0=127.88.0.2",
		"ifindex":  "eth0",
		"ifreq":    "127.88.0.2 en0-hidden",
		"unix":     "/probe.sock",
	} {
		if got[k] != want {
			t.Errorf("%s: %q, want %q (all: %v)", k, got[k], want, got)
		}
	}
	if fi, err := os.Stat(filepath.Join(root, "probe.sock")); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("the socket is not in the container's root: %v", err)
	}
}

// freePort returns a loopback port nothing listens on.
func freePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestProcessListingsHoldTheSandboxsOwn(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ps")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-o", bin, "testdata/ps/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	run := func(env ...string) string {
		cmd := exec.Command(bin)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ps: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// Each listing reports whether it holds the probe, its child, and launchd.
	if got := run(); !strings.Contains(got, "sysctl=111") {
		t.Fatalf("without the library: %q, want launchd listed", got)
	}
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := run("DYLD_INSERT_LIBRARIES=" + lib); got != "sysctl=110\nlibproc=110" {
		t.Errorf("with the library: %q, want the probe and its child alone", got)
	}
}

// Containers under private IPC have System V key spaces of their own: one
// key names an object of each container's.
func TestSystemVKeysPerContainer(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sysv")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-o", bin, "testdata/sysv/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(prefix string, args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"DYLD_INSERT_LIBRARIES=" + lib, "SANDBOX_IPC_PREFIX=" + prefix}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sysv: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	a, b := fmt.Sprintf("fwsa%d.", os.Getpid()), fmt.Sprintf("fwsb%d.", os.Getpid())
	t.Cleanup(func() { run(a, "remove"); run(b, "remove") })
	if got := run(a); got != "111" {
		t.Fatalf("the first container's objects: %q, want all three made", got)
	}
	if got := run(b); got != "111" {
		t.Errorf("another container's objects under the same key: %q, want all three made", got)
	}
	if got := run(a); got != "000" {
		t.Errorf("the first container again: %q, want its own objects found existing", got)
	}
}

// The container's main process is its pid 1.
func TestMainProcessIsPidOne(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "init")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-o", bin, "testdata/init/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Env = []string{"DYLD_INSERT_LIBRARIES=" + lib}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if want := "pid=1 ppid=0\nchild-ppid=1 kill1=0\ntable1=1\n"; string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

// A container listens on a port that has no host port, and reaches it,
// and a member's, through routers.
func TestPortsWithoutHostPorts(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "unexposed")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-o", bin, "testdata/unexposed/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	// A Unix socket's path is capped at 104 bytes.
	dir, err := os.MkdirTemp("/tmp", "fwsp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dir, _ = filepath.EvalSymlinks(dir)
	// The router gives the probe's UDP ports these flow ports, which its
	// sandbox lets it reach.
	flowPorts := []int{freePort(t, "udp"), freePort(t, "udp"), freePort(t, "udp")}
	var flows atomic.Int32
	r, err := router.Listen("127.0.0.1:0", dir, func() (*net.UDPConn, error) {
		i := flows.Add(1) - 1
		if int(i) >= len(flowPorts) {
			return nil, fmt.Errorf("flow port %d asked for, %d given", i+1, len(flowPorts))
		}
		return net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: flowPorts[i]})
	}, router.Network{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// The member's UDP socket on its port 7100, which answers each datagram
	// with "hello" through the router.
	ul, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: router.UDPSocketPath(dir, 7100), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	go func() {
		buf := make([]byte, 128)
		for {
			n, _, err := ul.ReadFromUnix(buf)
			if err != nil {
				return
			}
			if n < router.Header {
				continue
			}
			ul.WriteToUnix(append(append([]byte{}, buf[:router.Header]...), "hello"...), &net.UnixAddr{Name: router.UDPRouterPath(dir), Net: "unixgram"})
		}
	}()
	// The member's listener on its port 7100, which the router reaches as
	// a member's router would.
	l, err := net.Listen("unix", router.SocketPath(dir, 7100))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("hello"))
			c.Close()
		}
	}()
	network := filepath.Join(t.TempDir(), "network")
	rp := r.Addr().Port
	os.WriteFile(network, []byte(fmt.Sprintf("127.88.0.2\tapp\t\t%d\tproj_default\t0\n127.88.0.3\tpeer\t\t%d\tproj_default\t0\n", rp, rp)), 0o644)
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, dir, strconv.Itoa(rp), strconv.Itoa(flowPorts[0]), strconv.Itoa(flowPorts[1]), strconv.Itoa(flowPorts[2]))
	cmd.Env = []string{"DYLD_INSERT_LIBRARIES=" + lib, PortDirVar + "=" + dir, RouterVar + "=" + strconv.Itoa(rp),
		NetworkVar + "=" + network, AddressVar + "=127.88.0.2"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unexposed: %v\n%s", err, out)
	}
	want := "listen=[::]:7000\nlocal=ping 127.0.0.1:7000\npeer=hello 127.88.0.3:7100\nrefused=-1\n" +
		"ephemeral=ping in-range\nudp-ephemeral=in-range\n" +
		"udp-listen=[::]:7000\nudp-local=ping from-loopback\nudp-reply=pong 127.0.0.1:7000\n" +
		"udp-connect=0 peer-is-client\nudp-connected-send=send writ wriv\nudp-isconn=-1 EISCONN\nudp-filter=mine\n" +
		"udp-disconnect=0 ENOTCONN\nudp-disconnected=ot hr\nudp-pktinfo6=1 pk6 ::ffff:127.0.0.1\n" +
		"udp-dstaddr=127.0.0.1 pktinfo=127.0.0.1 truncated=yes\nudp-peer=hello 127.88.0.3:7100\n"
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}
