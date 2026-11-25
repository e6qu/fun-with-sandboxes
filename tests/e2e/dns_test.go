//go:build e2e && darwin

package e2e

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// resolvePy prints the IPv4 addresses getaddrinfo, or with "byname"
// gethostbyname, resolves its argument to, or "error" and the error.
const resolvePy = `import socket, sys
try:
    if len(sys.argv) > 2:
        print([socket.gethostbyname(sys.argv[1])])
    else:
        print(sorted({a[4][0] for a in socket.getaddrinfo(sys.argv[1], 80, socket.AF_INET)}))
except (socket.gaierror, socket.herror) as e:
    print("error", e)
`

// serveDNS answers DNS queries on UDP port 53 of this machine's network
// address from records, by lowercase absolute name, and with NXDOMAIN for
// any other name: a server the host's resolver does not ask. It reports
// false where the port cannot be had.
func serveDNS(t *testing.T, records map[string]netip.Addr) (string, bool) {
	t.Helper()
	addr := hostAddress(t)
	// macOS lets a process without root bind a port below 1024 only on
	// the wildcard address.
	pc, err := net.ListenPacket("udp4", ":53")
	if err != nil {
		t.Logf("no DNS server of the test's own: %v", err)
		return "", false
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var m dnsmessage.Message
			if m.Unpack(buf[:n]) != nil || len(m.Questions) != 1 {
				continue
			}
			q := m.Questions[0]
			m.Response, m.RecursionAvailable = true, true
			a, ok := records[strings.ToLower(q.Name.String())]
			if !ok {
				m.RCode = dnsmessage.RCodeNameError
			} else if q.Type == dnsmessage.TypeA {
				m.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					Body:   &dnsmessage.AResource{A: a.As4()},
				}}
			}
			if b, err := m.Pack(); err == nil {
				pc.WriteTo(b, from)
			}
		}
	}()
	return addr, true
}

// A container's DNS servers, search domains and options go into its
// resolv.conf as Docker writes them, and the names neither its hosts file
// nor its stack answers resolve through its servers, with its search
// domains.
func TestDNSSettings(t *testing.T) {
	out := mustDocker(t, "run", "--rm", "--dns", "1.1.1.1", "--dns", "1.0.0.1", "--dns-search", "corp.example", "--dns-option", "ndots:2",
		"fws/python:3.14", "python3", "-c", "print(open('/etc/resolv.conf').read(), end='')")
	var servers []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "nameserver" {
			servers = append(servers, f[1])
		}
	}
	if strings.Join(servers, " ") != "1.1.1.1 1.0.0.1" || !strings.Contains(out, "\nsearch corp.example\n") || !strings.Contains(out, "\noptions ndots:2\n") {
		t.Errorf("/etc/resolv.conf:\n%s", out)
	}
	if out := mustDocker(t, "run", "--rm", "--dns-search", ".", "fws/python:3.14", "python3", "-c", "print(open('/etc/resolv.conf').read(), end='')"); strings.Contains(out, "search ") || strings.Contains(out, "domain ") {
		t.Errorf("--dns-search . left a search domain:\n%s", out)
	}

	// A public server, through the container's router.
	if out := mustDocker(t, "run", "--rm", "--dns", "1.1.1.1", "fws/python:3.14", "python3", "-c", resolvePy, "one.one.one.one"); !strings.Contains(out, "'1.1.1.1'") {
		t.Errorf("one.one.one.one through 1.1.1.1: %q", out)
	}

	// The refusals, with their reasons.
	if out, err := docker(t, "create", "--dns", "127.0.0.1", "fws/python:3.14", "true"); err == nil || !strings.Contains(err.Error(), "DNS server 127.0.0.1 is not supported") {
		t.Errorf("--dns 127.0.0.1: %q, %v", out, err)
		if err == nil {
			docker(t, "rm", strings.TrimSpace(out))
		}
	}

	server, ok := serveDNS(t, map[string]netip.Addr{
		"web.fws-e2e.test.":  netip.MustParseAddr("192.0.2.77"),
		"only.fws-e2e.test.": netip.MustParseAddr("192.0.2.78"),
	})
	if !ok {
		return
	}
	run := func(args ...string) string {
		t.Helper()
		return mustDocker(t, append([]string{"run", "--rm", "--dns", server}, args...)...)
	}
	// Names only the container's server knows: in full, through a search
	// domain, by gethostbyname, and one it does not know.
	if out := run("fws/python:3.14", "python3", "-c", resolvePy, "only.fws-e2e.test"); out != "['192.0.2.78']\n" {
		t.Errorf("only.fws-e2e.test: %q", out)
	}
	if out := run("--dns-search", "fws-e2e.test", "fws/python:3.14", "python3", "-c", resolvePy, "web"); out != "['192.0.2.77']\n" {
		t.Errorf("web in search domain fws-e2e.test: %q", out)
	}
	if out := run("--dns-search", "fws-e2e.test", "fws/python:3.14", "python3", "-c", resolvePy, "web", "byname"); out != "['192.0.2.77']\n" {
		t.Errorf("gethostbyname of web in search domain fws-e2e.test: %q", out)
	}
	if out := run("fws/python:3.14", "python3", "-c", resolvePy, "missing.fws-e2e.test"); !strings.HasPrefix(out, "error") {
		t.Errorf("a name the container's server does not know resolved: %q", out)
	}
	// Without internet access the server is out of reach.
	if out := run("--network", "none", "fws/python:3.14", "python3", "-c", resolvePy, "only.fws-e2e.test"); !strings.HasPrefix(out, "error") {
		t.Errorf("a container with network none resolved through its server: %q", out)
	}

	// The stack's members still answer to their names.
	mustDocker(t, "network", "create", "e2e-dns")
	defer docker(t, "network", "rm", "e2e-dns")
	mustDocker(t, "run", "-d", "--name", "e2e-dns-db", "--network", "e2e-dns", "fws/python:3.14", "python3", "-c", "import time; time.sleep(600)")
	defer docker(t, "rm", "-f", "e2e-dns-db")
	if out := run("--network", "e2e-dns", "--dns-search", "fws-e2e.test", "fws/python:3.14", "python3", "-c", resolvePy, "e2e-dns-db"); strings.HasPrefix(out, "error") || strings.Contains(out, "192.0.2.") {
		t.Errorf("a member's name with DNS settings of the container's own: %q", out)
	}
}

// asyncPy resolves its argument's IPv4 addresses three ways and prints
// each sorted: getaddrinfo; libinfo's getaddrinfo_async_start, whose
// result arrives as a message on a Mach port, which CFNetwork uses; and
// dns_sd's DNSServiceGetAddrInfo, which mDNSResponder serves.
const asyncPy = `import ctypes, socket, sys
name = sys.argv[1].encode()
lib = ctypes.CDLL(None)
print("getaddrinfo", sorted({a[4][0] for a in socket.getaddrinfo(sys.argv[1], 80, socket.AF_INET, socket.SOCK_STREAM)}))

class addrinfo(ctypes.Structure):
    pass
addrinfo._fields_ = [("flags", ctypes.c_int), ("family", ctypes.c_int), ("socktype", ctypes.c_int), ("protocol", ctypes.c_int),
    ("addrlen", ctypes.c_uint32), ("canonname", ctypes.c_char_p), ("addr", ctypes.c_void_p), ("next", ctypes.POINTER(addrinfo))]
GaiCallback = ctypes.CFUNCTYPE(None, ctypes.c_int32, ctypes.POINTER(addrinfo), ctypes.c_void_p)
found = set()
def gai_done(status, res, ctx):
    if status != 0:
        found.add("error %d" % status)
        return
    a = res
    while a:
        found.add(socket.inet_ntop(socket.AF_INET, ctypes.string_at(a.contents.addr, 8)[4:8]))
        a = a.contents.next
    lib.freeaddrinfo(res)
gai_cb = GaiCallback(gai_done)
hints = addrinfo(family=socket.AF_INET, socktype=socket.SOCK_STREAM)
port = ctypes.c_uint32()
lib.getaddrinfo_async_start.argtypes = [ctypes.POINTER(ctypes.c_uint32), ctypes.c_char_p, ctypes.c_char_p, ctypes.POINTER(addrinfo), GaiCallback, ctypes.c_void_p]
assert lib.getaddrinfo_async_start(ctypes.byref(port), name, b"80", ctypes.byref(hints), gai_cb, None) == 0
buf = ctypes.create_string_buffer(4096)
ctypes.memmove(buf, (ctypes.c_uint32 * 4)(0, 4096, 0, port.value), 16)
MACH_RCV_MSG, MACH_RCV_TIMEOUT = 2, 0x100
assert lib.mach_msg(buf, MACH_RCV_MSG | MACH_RCV_TIMEOUT, 0, 4096, port.value, 60000, 0) == 0
lib.getaddrinfo_async_handle_reply(buf)
print("async", sorted(found))

DnsCallback = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_uint32, ctypes.c_uint32, ctypes.c_int32, ctypes.c_char_p, ctypes.c_void_p, ctypes.c_uint32, ctypes.c_void_p)
answers = set()
def dns_answer(ref, flags, iface, err, host, addr, ttl, ctx):
    answers.add(socket.inet_ntop(socket.AF_INET, ctypes.string_at(addr, 8)[4:8]) if err == 0 else "error %d" % err)
dns_cb = DnsCallback(dns_answer)
ref = ctypes.c_void_p()
lib.DNSServiceGetAddrInfo.argtypes = [ctypes.POINTER(ctypes.c_void_p), ctypes.c_uint32, ctypes.c_uint32, ctypes.c_uint32, ctypes.c_char_p, DnsCallback, ctypes.c_void_p]
lib.DNSServiceProcessResult.argtypes = [ctypes.c_void_p]
lib.DNSServiceRefDeallocate.argtypes = [ctypes.c_void_p]
assert lib.DNSServiceGetAddrInfo(ctypes.byref(ref), 0, 0, 1, name, dns_cb, None) == 0
assert lib.DNSServiceProcessResult(ref) == 0
lib.DNSServiceRefDeallocate(ref)
print("dns_sd", sorted(answers))
`

// The asynchronous lookups answer what getaddrinfo answers: a member of the
// container's stack, and a name its hosts file lists.
func TestAsyncLookups(t *testing.T) {
	mustDocker(t, "network", "create", "e2e-async")
	defer docker(t, "network", "rm", "e2e-async")
	mustDocker(t, "run", "-d", "--name", "e2e-async-db", "--network", "e2e-async", "fws/python:3.14", "python3", "-c", "import time; time.sleep(600)")
	defer docker(t, "rm", "-f", "e2e-async-db")
	for name, want := range map[string]string{"e2e-async-db": "", "listed.fws-e2e.test": "192.0.2.90"} {
		out := mustDocker(t, "run", "--rm", "--network", "e2e-async", "--add-host", "listed.fws-e2e.test:192.0.2.90", "fws/python:3.14", "python3", "-c", asyncPy, name)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 3 {
			t.Errorf("%s: %q", name, out)
			continue
		}
		addrs := strings.TrimPrefix(lines[0], "getaddrinfo ")
		if want != "" && addrs != "['"+want+"']" || strings.Contains(addrs, "error") || addrs == "[]" {
			t.Errorf("%s through getaddrinfo: %s", name, addrs)
		}
		if lines[1] != "async "+addrs || lines[2] != "dns_sd "+addrs {
			t.Errorf("%s: the asynchronous lookups differ from getaddrinfo's %s:\n%s", name, addrs, out)
		}
	}
}

// dnsPy asks the first server of the container's resolv.conf for its
// argument's A records itself, as dig does: by default one query in a
// datagram from an unconnected socket, whose answer has to come from the
// server asked; with "tcp", over a TCP connection to the server, as a
// resolver retries a truncated answer. An IPv6 server is asked over IPv6.
// It prints the answer's code and its addresses, sorted.
const dnsPy = `import socket, struct, sys
ns = [l.split()[1] for l in open('/etc/resolv.conf') if l.startswith('nameserver')][0]
fam = socket.AF_INET6 if ':' in ns else socket.AF_INET
q = struct.pack('>HHHHHH', 0x1234, 0x0100, 1, 0, 0, 0)
q += b''.join(bytes([len(p)]) + p.encode() for p in sys.argv[1].split('.')) + b'\0' + struct.pack('>HH', 1, 1)
if len(sys.argv) > 2 and sys.argv[2] == 'tcp':
    s = socket.socket(fam, socket.SOCK_STREAM)
    s.settimeout(20)
    s.connect((ns, 53))
    peer = s.getpeername()
    s.sendall(struct.pack('>H', len(q)) + q)
    def take(n):
        b = b''
        while len(b) < n:
            c = s.recv(n - len(b))
            if not c:
                raise SystemExit('closed')
            b += c
        return b
    r = take(struct.unpack('>H', take(2))[0])
    src = peer
else:
    s = socket.socket(fam, socket.SOCK_DGRAM)
    s.settimeout(20)
    s.sendto(q, (ns, 53))
    r, src = s.recvfrom(4096)
if tuple(src[:2]) != (ns, 53):
    print('answer from', src, 'not', ns)
    sys.exit()
an = struct.unpack('>H', r[6:8])[0]
i = 12
while r[i]:
    i += r[i] + 1
i += 5
out = []
for _ in range(an):
    if r[i] & 0xc0 == 0xc0:
        i += 2
    else:
        while r[i]:
            i += r[i] + 1
        i += 1
    t, c, ttl, l = struct.unpack('>HHIH', r[i:i+10])
    i += 10
    if t == 1:
        out.append(socket.inet_ntoa(r[i:i+4]))
    i += l
print(r[3] & 15, sorted(out))
`

// A program that sends DNS itself, to its resolv.conf's server, has its
// container's router answer: its stack's names by the router, others
// through the container's servers, given internet access.
func TestDNSQueries(t *testing.T) {
	mustDocker(t, "network", "create", "e2e-dnsq")
	defer docker(t, "network", "rm", "e2e-dnsq")
	mustDocker(t, "run", "-d", "--name", "e2e-dnsq-db", "--network", "e2e-dnsq", "--network-alias", "database", "fws/python:3.14", "python3", "-c", "import time; time.sleep(600)")
	defer docker(t, "rm", "-f", "e2e-dnsq-db")
	db := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", "e2e-dnsq-db"))
	for _, name := range []string{"e2e-dnsq-db", "database"} {
		if out := mustDocker(t, "run", "--rm", "--network", "e2e-dnsq", "fws/python:3.14", "python3", "-c", dnsPy, name); out != "0 ['"+db+"']\n" {
			t.Errorf("%s: %q, want its address %s", name, out, db)
		}
	}

	// Over TCP, as a resolver retries a truncated answer, and to a server
	// at an IPv6 address, over UDP and TCP: the router answers each.
	if out := mustDocker(t, "run", "--rm", "--network", "e2e-dnsq", "fws/python:3.14", "python3", "-c", dnsPy, "e2e-dnsq-db", "tcp"); out != "0 ['"+db+"']\n" {
		t.Errorf("e2e-dnsq-db over TCP: %q, want its address %s", out, db)
	}
	for _, how := range []string{"udp", "tcp"} {
		if out := mustDocker(t, "run", "--rm", "--network", "e2e-dnsq", "--dns", "2606:4700:4700::1111", "fws/python:3.14", "python3", "-c", dnsPy, "e2e-dnsq-db", how); out != "0 ['"+db+"']\n" {
			t.Errorf("e2e-dnsq-db from an IPv6 server over %s: %q, want its address %s", how, out, db)
		}
	}
	if out := mustDocker(t, "run", "--rm", "--dns", "1.1.1.1", "fws/python:3.14", "python3", "-c", dnsPy, "one.one.one.one", "tcp"); out != "0 ['1.0.0.1', '1.1.1.1']\n" {
		t.Errorf("one.one.one.one through 1.1.1.1 over TCP: %q", out)
	}

	// Other names go on to the container's servers: a public one, as
	// TestDNSSettings uses, since a test's own server on port 53 needs root.
	if out := mustDocker(t, "run", "--rm", "--dns", "1.1.1.1", "fws/python:3.14", "python3", "-c", dnsPy, "one.one.one.one"); out != "0 ['1.0.0.1', '1.1.1.1']\n" {
		t.Errorf("one.one.one.one through 1.1.1.1: %q", out)
	}
	if out := mustDocker(t, "run", "--rm", "--dns", "1.1.1.1", "fws/python:3.14", "python3", "-c", dnsPy, "missing.fws-e2e.invalid"); out != "3 []\n" {
		t.Errorf("missing.fws-e2e.invalid: %q, want NXDOMAIN", out)
	}
	// Without internet access, what the stack does not answer is refused.
	mustDocker(t, "network", "create", "--internal", "e2e-dnsq-internal")
	defer docker(t, "network", "rm", "e2e-dnsq-internal")
	if out := mustDocker(t, "run", "--rm", "--network", "e2e-dnsq-internal", "--dns", "1.1.1.1", "fws/python:3.14", "python3", "-c", dnsPy, "one.one.one.one"); out != "5 []\n" {
		t.Errorf("one.one.one.one without internet access: %q, want REFUSED", out)
	}
}

// Network.framework, through which an app's URLSession connects, reaches a
// container on the same network by its name, as getaddrinfo does.
func TestNetworkFrameworkLookups(t *testing.T) {
	dir, err := os.MkdirTemp("", "fws-e2e-nw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-Werror", "-framework", "Network", "-o", filepath.Join(dir, "nwprobe"),
		"../../internal/pathmap/testdata/nwprobe/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	mustDocker(t, "network", "create", "e2e-nw")
	defer docker(t, "network", "rm", "e2e-nw")
	mustDocker(t, "run", "-d", "--name", "e2e-nw-web", "--network", "e2e-nw", "fws/python:3.14", "python3", "-m", "http.server", "8000")
	defer docker(t, "rm", "-f", "e2e-nw-web")
	var out string
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		out, _ = docker(t, "run", "--rm", "--network", "e2e-nw", "-v", dir+":/app", "fws/base:1", "/app/nwprobe", "e2e-nw-web", "8000")
		if strings.HasPrefix(out, "ready ") {
			return
		}
	}
	t.Errorf("Network.framework, connecting to e2e-nw-web: %q", out)
}

// A DNS server a container of the stack runs serves the stack: a
// container whose --dns names it resolves through it, by the system's
// resolver and by a program that sends DNS itself, on a network without
// internet access, while the stack's own names stay the stack's.
func TestStackDNSServer(t *testing.T) {
	const network, server = "fws-e2e-stackdns", "fws-e2e-stackdns-server"
	mustDocker(t, "network", "create", "--internal", "--subnet", "172.29.53.0/24", network)
	t.Cleanup(func() { docker(t, "network", "rm", network) })
	mustDocker(t, "run", "-d", "--name", server, "--network", network, "--ip", "172.29.53.53",
		"--expose", "53/udp", "--expose", "53/tcp", "--no-healthcheck", "fws/toolbox:1", "dnsserve", "192.0.2.53")
	t.Cleanup(func() { docker(t, "rm", "-f", server) })
	waitLog(t, server, "serving DNS")
	out, err := docker(t, "run", "--rm", "--network", network, "--dns", "172.29.53.53", "--no-healthcheck",
		"fws/toolbox:1", "lookup", "www.example.test")
	if err != nil || strings.TrimSpace(out) != "192.0.2.53 192.0.2.53" {
		t.Errorf("a name through the stack's DNS server: %q, %v", out, err)
	}
	out, err = docker(t, "run", "--rm", "--network", network, "--dns", "172.29.53.53", "--no-healthcheck",
		"fws/toolbox:1", "lookup", server)
	// The system resolver asks the router, which knows its stack's names;
	// a program sending DNS to the server itself gets the server's answer,
	// as dig @server would.
	if err != nil || !strings.HasPrefix(out, "172.29.53.53 ") {
		t.Errorf("a member's name with the stack's DNS server: %q, %v", out, err)
	}
}
