//go:build e2e && darwin

package e2e

import (
	"net"
	"net/netip"
	"strings"
	"testing"

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
	pc, err := net.ListenPacket("udp4", net.JoinHostPort(addr, "53"))
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
