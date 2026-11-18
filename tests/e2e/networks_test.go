//go:build e2e && darwin

package e2e

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

// netEndpoint is a container's endpoint on a network as docker inspect
// prints it, an absent address as "".
type netEndpoint struct {
	IPAddress, Gateway, GlobalIPv6Address, IPv6Gateway string
	IPPrefixLen, GlobalIPv6PrefixLen                   int
}

func endpointOn(t *testing.T, container, network string) netEndpoint {
	t.Helper()
	var nets map[string]netEndpoint
	if err := json.Unmarshal([]byte(mustDocker(t, "inspect", "-f", "{{json .NetworkSettings.Networks}}", container)), &nets); err != nil {
		t.Fatal(err)
	}
	return nets[network]
}

// netInspect is what docker network inspect prints of a network's
// address families and IPAM configuration.
type netInspect struct {
	EnableIPv4, EnableIPv6 bool
	IPAM                   struct {
		Config []struct{ Subnet, Gateway, IPRange string }
	}
}

func inspectNetwork(t *testing.T, network string) netInspect {
	t.Helper()
	var out []netInspect
	if err := json.Unmarshal([]byte(mustDocker(t, "network", "inspect", network)), &out); err != nil || len(out) != 1 {
		t.Fatalf("network inspect %s: %v", network, err)
	}
	return out[0]
}

// serveHTTP runs a Python HTTP server on port 8000 on IPv4's and IPv6's
// wildcard addresses, healthy once it answers on localhost.
func serveHTTP(t *testing.T, name string, args ...string) {
	t.Helper()
	waitedRun(t, name, append(args, "--expose", "8000",
		"--health-cmd", `python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:8000')"`,
		"--health-interval", "1s", "--health-start-interval", "200ms", "--health-start-period", "30s",
		"fws/python:3.14", "python3", "-m", "http.server", "--bind", "::", "8000")...)
}

// A network with several subnets of each family reports them all, IPv4
// first, as Docker does, and gives each container an address of each
// family from the first subnet with room, or from the one that holds the
// address it names, with that subnet's gateway; its members reach one
// another at addresses of any of them.
func TestNetworkWithSeveralSubnets(t *testing.T) {
	const network = "e2e-multi"
	docker(t, "network", "rm", network)
	mustDocker(t, "network", "create", "--ipv6",
		"--subnet", "10.71.1.0/30", "--subnet", "10.71.2.0/24", "--gateway", "10.71.2.254",
		"--subnet", "fd00:71:1::/126", "--subnet", "fd00:71:2::/64", network)
	t.Cleanup(func() { docker(t, "network", "rm", network) })

	// The Docker CLI gathers --subnet options in a map, so the engine sees
	// each family's in no certain order: which of a family comes first is
	// the unit tests' to check (TestSeveralSubnets).
	pools := map[string]netEndpoint{
		"10.71.1.0/30":    {Gateway: "10.71.1.1", IPPrefixLen: 30},
		"10.71.2.0/24":    {Gateway: "10.71.2.254", IPPrefixLen: 24},
		"fd00:71:1::/126": {IPv6Gateway: "fd00:71:1::1", GlobalIPv6PrefixLen: 126},
		"fd00:71:2::/64":  {IPv6Gateway: "fd00:71:2::1", GlobalIPv6PrefixLen: 64},
	}
	in := inspectNetwork(t, network)
	seen6 := false
	for _, c := range in.IPAM.Config {
		want, ok := pools[c.Subnet]
		is6 := strings.Contains(c.Subnet, ":")
		if !ok || c.Gateway != want.Gateway+want.IPv6Gateway || (!is6 && seen6) {
			t.Errorf("network inspect lists %s with gateway %s, IPv4 first: %+v", c.Subnet, c.Gateway, in.IPAM.Config)
		}
		seen6 = seen6 || is6
	}
	if len(in.IPAM.Config) != len(pools) || !in.EnableIPv4 || !in.EnableIPv6 {
		t.Errorf("network inspect: %v %v %+v", in.EnableIPv4, in.EnableIPv6, in.IPAM.Config)
	}
	// within reports whether the endpoint's address of a family lies in
	// one of the network's subnets, with that subnet's gateway and length.
	within := func(e netEndpoint) bool {
		ok4, ok6 := false, false
		for subnet, p := range pools {
			prefix := netip.MustParsePrefix(subnet)
			if a, err := netip.ParseAddr(e.IPAddress); err == nil && prefix.Contains(a) {
				ok4 = e.Gateway == p.Gateway && e.IPPrefixLen == p.IPPrefixLen
			}
			if a, err := netip.ParseAddr(e.GlobalIPv6Address); err == nil && prefix.Contains(a) {
				ok6 = e.IPv6Gateway == p.IPv6Gateway && e.GlobalIPv6PrefixLen == p.GlobalIPv6PrefixLen
			}
		}
		return ok4 && ok6
	}

	serveHTTP(t, "e2e-multi-api", "--network", network)
	mustDocker(t, "run", "-d", "--name", "e2e-multi-client", "--network", network, "fws/python:3.14", "python3", "-c", "import time; time.sleep(600)")
	t.Cleanup(func() { docker(t, "rm", "-f", "e2e-multi-client") })
	mustDocker(t, "run", "-d", "--name", "e2e-multi-static", "--network", network, "--ip", "10.71.2.100", "--ip6", "fd00:71:2::100", "fws/python:3.14", "python3", "-c", "import time; time.sleep(600)")
	t.Cleanup(func() { docker(t, "rm", "-f", "e2e-multi-static") })
	api, client := endpointOn(t, "e2e-multi-api", network), endpointOn(t, "e2e-multi-client", network)
	if !within(api) || !within(client) || api.IPAddress == client.IPAddress || api.GlobalIPv6Address == client.GlobalIPv6Address {
		t.Errorf("the endpoints: %+v and %+v", api, client)
	}
	// A named address comes from the subnet that holds it.
	if got, want := endpointOn(t, "e2e-multi-static", network), (netEndpoint{IPAddress: "10.71.2.100", IPPrefixLen: 24, Gateway: "10.71.2.254", GlobalIPv6Address: "fd00:71:2::100", GlobalIPv6PrefixLen: 64, IPv6Gateway: "fd00:71:2::1"}); got != want {
		t.Errorf("e2e-multi-static's endpoint: %+v, want %+v", got, want)
	}

	// Members reach one another across the subnets, by name and by either
	// family's address; a member with nothing listening refuses.
	api4, api6, client4 := api.IPAddress+":8000", api.GlobalIPv6Address+":8000", client.IPAddress+":8000"
	out := mustDocker(t, "exec", "e2e-multi-static", "python3", "-c", sharedClient, "e2e-multi-api:8000", api4, api6, client4)
	if want := "e2e-multi-api:8000 HTTP/1.0 200 OK\n" + api4 + " HTTP/1.0 200 OK\n" + api6 + " HTTP/1.0 200 OK\n" + client4 + " ConnectionRefusedError\n"; out != want {
		t.Errorf("a member in another subnet printed %q, want %q", out, want)
	}
	if _, err := docker(t, "run", "--rm", "--network", network, "--ip6", api.GlobalIPv6Address, "fws/python:3.14", "python3", "-c", "pass"); err == nil || !strings.Contains(err.Error(), "Address already in use") {
		t.Errorf("a taken IPv6 address: %v", err)
	}
	if _, err := docker(t, "run", "--rm", "--network", network, "--ip", "10.71.3.5", "fws/python:3.14", "python3", "-c", "pass"); err == nil || !strings.Contains(err.Error(), "no configured subnet") {
		t.Errorf("an address in none of the subnets: %v", err)
	}
}

// An IPv6-only network gives its containers an IPv6 address alone, which
// inspect and their hosts files report and their names resolve to, and its
// members reach one another there over TCP and UDP, as on Docker.
func TestIPv6OnlyNetwork(t *testing.T) {
	const network = "e2e-net6"
	docker(t, "network", "rm", network)
	mustDocker(t, "network", "create", "--ipv4=false", "--ipv6", "--subnet", "fd00:72::/64", network)
	t.Cleanup(func() { docker(t, "network", "rm", network) })

	in := inspectNetwork(t, network)
	if in.EnableIPv4 || !in.EnableIPv6 || len(in.IPAM.Config) != 1 || in.IPAM.Config[0].Subnet != "fd00:72::/64" || in.IPAM.Config[0].Gateway != "fd00:72::1" {
		t.Errorf("network inspect: %+v", in)
	}
	serveHTTP(t, "e2e-net6-api", "--network", network)
	if got, want := endpointOn(t, "e2e-net6-api", network), (netEndpoint{GlobalIPv6Address: "fd00:72::2", GlobalIPv6PrefixLen: 64, IPv6Gateway: "fd00:72::1"}); got != want {
		t.Errorf("the api's endpoint: %+v, want %+v", got, want)
	}
	udp := "e2e-net6-udp"
	mustDocker(t, "run", "-d", "--name", udp, "--network", network, "fws/python:3.14", "python3", "-c", `import socket
u = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM)
u.bind(("::", 9053))
print("ready", flush=True)
while True:
    d, a = u.recvfrom(512)
    u.sendto(d.upper(), a)`)
	t.Cleanup(func() { docker(t, "rm", "-f", udp) })
	waitLog(t, udp, "ready")

	out := mustDocker(t, "run", "--rm", "--network", network, "fws/python:3.14", "python3", "-c", `import ctypes, socket
hosts = open("/etc/hosts").read()
print("own", "fd00:72::4\t" in hosts, "127.88." in hosts)
# The container's interfaces, as getifaddrs lists them: its IPv6 address
# alone.
class sockaddr(ctypes.Structure):
    _fields_ = [("len", ctypes.c_uint8), ("family", ctypes.c_uint8), ("data", ctypes.c_char * 26)]
class ifaddrs(ctypes.Structure):
    pass
ifaddrs._fields_ = [("next", ctypes.POINTER(ifaddrs)), ("name", ctypes.c_char_p), ("flags", ctypes.c_uint),
    ("addr", ctypes.POINTER(sockaddr)), ("mask", ctypes.POINTER(sockaddr)), ("dst", ctypes.POINTER(sockaddr)), ("data", ctypes.c_void_p)]
libc = ctypes.CDLL(None)
head = ctypes.POINTER(ifaddrs)()
assert libc.getifaddrs(ctypes.byref(head)) == 0
found, a = [], head
while a:
    i = a.contents
    if i.addr and i.name.startswith(b"eth"):
        sa = i.addr.contents
        # A c_char array stops at its first NUL; the address's bytes do not.
        raw = ctypes.string_at(i.addr, 28)
        if sa.family == socket.AF_INET:
            found.append(socket.inet_ntop(socket.AF_INET, raw[4:8]))
        elif sa.family == socket.AF_INET6:
            found.append(socket.inet_ntop(socket.AF_INET6, raw[8:24]))
    a = i.next
print("interfaces", sorted(found))
print(sorted({a[4][0] for a in socket.getaddrinfo("e2e-net6-api", 8000, socket.AF_UNSPEC, socket.SOCK_STREAM)}))
try:
    socket.getaddrinfo("e2e-net6-api", 8000, socket.AF_INET)
    print("an IPv4 address")
except socket.gaierror:
    print("no IPv4 address")
s = socket.create_connection(("e2e-net6-api", 8000), timeout=10)
s.sendall(b"GET / HTTP/1.0\r\nHost: x\r\n\r\n")
print(s.recv(4096).split(b"\r\n", 1)[0].decode(), s.getpeername()[0], s.getsockname()[0])
u = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM)
u.settimeout(10)
u.sendto(b"ping", ("e2e-net6-udp", 9053))
d, a = u.recvfrom(512)
print(d.decode(), a[0])`)
	if want := "own True False\ninterfaces ['fd00:72::4']\n['fd00:72::2']\nno IPv4 address\nHTTP/1.0 200 OK fd00:72::2 fd00:72::4\nPING fd00:72::3\n"; out != want {
		t.Errorf("a client on %s printed:\n%s\nwant:\n%s", network, out, want)
	}
	if _, err := docker(t, "run", "--rm", "--network", network, "--ip", "10.72.0.5", "fws/python:3.14", "python3", "-c", "pass"); err == nil || !strings.Contains(err.Error(), "IPv6-only") {
		t.Errorf("an IPv4 address on an IPv6-only network: %v", err)
	}
	if _, err := docker(t, "network", "create", "--ipv4=false", "e2e-net-neither"); err == nil || !strings.Contains(err.Error(), "IPv4 or IPv6 must be enabled") {
		docker(t, "network", "rm", "e2e-net-neither")
		t.Errorf("a network of neither family: %v", err)
	}
}
