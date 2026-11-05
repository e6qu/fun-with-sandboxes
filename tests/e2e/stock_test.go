//go:build e2e && darwin

package e2e

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Stock servers listen on their own ports, on the wildcard address and
// with dual-stack sockets, as they do in Docker, and a health check inside
// the container reaches them on localhost.
func TestStockServersOnTheirOwnPorts(t *testing.T) {
	for _, tc := range []struct {
		name, image, port, health, body string
		cmd                             []string
		// lo6 connects to ::1 and prints the peer's address.
		lo6 []string
	}{
		{
			name: "python", image: "fws/python:3.14", port: "8000", body: "Directory listing",
			health: `python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:8000')"`,
			cmd:    []string{"python3", "-m", "http.server", "8000"},
			lo6:    []string{"python3", "-c", "import socket; print(socket.create_connection(('::1', 8000)).getpeername()[0])"},
		},
		{
			name: "node", image: "fws/node:26", port: "3000", body: "from node",
			health: `node -e "fetch('http://localhost:3000').then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))"`,
			cmd:    []string{"node", "-e", "require('http').createServer((q, r) => r.end('from node')).listen(3000)"},
			lo6:    []string{"node", "-e", "const c = require('net').connect({host: '::1', port: 3000}, () => { console.log(c.remoteAddress); c.end() })"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "fws-e2e-stock-" + tc.name
			waitedRun(t, name, append([]string{"-p", tc.port, "--health-cmd", tc.health, "--health-interval", "1s", "--health-start-interval", "200ms", "--health-start-period", "30s", tc.image}, tc.cmd...)...)
			addr := strings.TrimSpace(mustDocker(t, "port", name, tc.port))
			resp, err := http.Get("http://" + addr + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil || !strings.Contains(string(b), tc.body) {
				t.Fatalf("GET through the published port: %q, %v", b, err)
			}
			// The dual-stack listener takes a client of IPv6 loopback.
			if out := strings.TrimSpace(mustDocker(t, append([]string{"exec", name}, tc.lo6...)...)); out != "::1" {
				t.Fatalf("a client of ::1 saw its peer as %q, want ::1", out)
			}
		})
	}
}

// A stack's members reach one another by service and container name, on
// their own ports, as containers on a Compose network do.
func TestStackMembersByName(t *testing.T) {
	const project = "e2emembers"
	labels := func(service string) []string {
		return []string{"--label", "com.docker.compose.project=" + project, "--label", "com.docker.compose.service=" + service}
	}
	web := "fws-e2e-members-web"
	waitedRun(t, web, append(append(labels("web"), "--expose", "8000",
		"--health-cmd", `python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:8000')"`,
		"--health-interval", "1s", "--health-start-interval", "200ms", "--health-start-period", "30s",
		"fws/python:3.14"), "python3", "-m", "http.server", "8000")...)
	client := `import socket, urllib.request
for name in ("web", "` + web + `"):
    print(name, socket.gethostbyname(name), urllib.request.urlopen("http://%s:8000" % name).status)
try:
    socket.create_connection(("web", 9999), timeout=5)
    print("closed port open")
except ConnectionRefusedError:
    print("closed port refused")
`
	out := mustDocker(t, append(append([]string{"run", "--rm"}, labels("client")...), "fws/python:3.14", "python3", "-c", client)...)
	for _, want := range []string{"web 127.88.", web + " 127.88.", " 200\n", "closed port refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("the client printed %q, want %q in it", out, want)
		}
	}
	// A container of another stack does not resolve the name.
	if out, err := docker(t, "run", "--rm", "fws/python:3.14", "python3", "-c", `import socket; socket.gethostbyname("web")`); err == nil {
		t.Errorf("a container outside the stack resolved web: %s", out)
	}
}

// A container listens on a port it neither exposes nor publishes, as any
// container may, and reaches it on localhost; a member of its stack
// reaches it by name.
func TestPortsNobodyExposes(t *testing.T) {
	const project = "e2eunexposed"
	labels := func(service string) []string {
		return []string{"--label", "com.docker.compose.project=" + project, "--label", "com.docker.compose.service=" + service}
	}
	// web serves HTTP on TCP port 8002 and echoes datagrams, upper-cased, on
	// UDP port 8004; it exposes neither.
	web := `import http.server, socket, threading
u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
u.bind(("0.0.0.0", 8004))
def echo():
    while True:
        d, a = u.recvfrom(2048)
        u.sendto(d.upper(), a)
threading.Thread(target=echo, daemon=True).start()
http.server.ThreadingHTTPServer(("", 8002), http.server.SimpleHTTPRequestHandler).serve_forever()
`
	waitedRun(t, "fws-e2e-unexposed-web", append(append(labels("web"),
		"--health-cmd", `python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:8002')"`,
		"--health-interval", "1s", "--health-start-interval", "200ms", "--health-start-period", "30s",
		"fws/python:3.14"), "python3", "-c", web)...)
	client := `import socket, urllib.request
print(urllib.request.urlopen("http://web:8002").status)
s = socket.create_connection(("web", 8002), timeout=5)
print(s.getpeername())
try:
    socket.create_connection(("web", 8003), timeout=5)
    print("closed port open")
except ConnectionRefusedError:
    print("closed port refused")
u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
u.settimeout(5)
u.sendto(b"from a peer", ("web", 8004))
print(u.recvfrom(2048))
`
	out := mustDocker(t, append(append([]string{"run", "--rm"}, labels("client")...), "fws/python:3.14", "python3", "-c", client)...)
	for _, want := range []string{"200\n", "('127.88.", ", 8002)", "closed port refused", "(b'FROM A PEER', ('127.88.", ", 8004))"} {
		if !strings.Contains(out, want) {
			t.Errorf("the client printed %q, want %q in it", out, want)
		}
	}
	// The container reaches its own UDP port on loopback.
	local := `import socket
u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
u.settimeout(5)
u.sendto(b"from inside", ("127.0.0.1", 8004))
print(u.recvfrom(2048))
`
	if out := strings.TrimSpace(mustDocker(t, "exec", "fws-e2e-unexposed-web", "python3", "-c", local)); out != "(b'FROM INSIDE', ('127.0.0.1', 8004))" {
		t.Errorf("the container's own client printed %q", out)
	}
}

// A container on another's network listens on that one's ports, as a
// process of the same network namespace: the other's published port and its
// own clients of localhost reach the sidecar's server.
func TestContainerNetworkMode(t *testing.T) {
	const web, side = "fws-e2e-netns-web", "fws-e2e-netns-side"
	waitedRun(t, web, "-p", "127.0.0.1::8000", "fws/toolbox:1")
	waitedRun(t, side, "--network", "container:"+web,
		"--health-cmd", `python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:8000')"`,
		"--health-interval", "1s", "--health-start-interval", "200ms", "--health-start-period", "30s",
		"fws/python:3.14", "python3", "-m", "http.server", "8000")
	addr := strings.TrimSpace(mustDocker(t, "port", web, "8000"))
	if body := httpGet(t, "http://"+addr+"/"); !strings.Contains(body, "Directory listing") {
		t.Errorf("web's published port reached %q, want the sidecar's server", body)
	}
	if out := strings.TrimSpace(mustDocker(t, "exec", web, "/toolbox", "fetch", "http://127.0.0.1:8000/")); out != "200 OK" {
		t.Errorf("web's client of localhost:8000 got %q, want the sidecar's server", out)
	}
	id := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.Id}}", web))
	if mode := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.HostConfig.NetworkMode}}", side)); mode != "container:"+id {
		t.Errorf("the sidecar's network mode is %q, want container:%s", mode, id)
	}
	if host := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.Config.Hostname}}", side)); host != id[:12] {
		t.Errorf("the sidecar's hostname is %q, want web's %q", host, id[:12])
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-p", "8001"}, "conflicting options: port publishing and the container type network mode"},
		{[]string{"--hostname", "box"}, "conflicting options: hostname and the network mode"},
		{[]string{"--add-host", "db:127.0.0.1"}, "conflicting options: custom host-to-IP mapping and the network mode"},
	} {
		args := append(append([]string{"run", "--rm", "--network", "container:" + web}, c.args...), "fws/toolbox:1", "echo")
		if _, err := docker(t, args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
	// A container whose network's container is not running cannot start.
	const late = "fws-e2e-netns-late"
	mustDocker(t, "create", "--name", late, "--network", "container:"+web, "fws/toolbox:1", "echo")
	defer docker(t, "rm", "-f", late)
	mustDocker(t, "stop", side)
	mustDocker(t, "stop", web)
	if _, err := docker(t, "start", late); err == nil || !strings.Contains(err.Error(), "cannot join network namespace of a non running container") {
		t.Errorf("start on a stopped container's network: %v", err)
	}
}

// sharedClient prints, for each host:port it names, the status line of an
// HTTP request to it, or the name of the error that stopped the request.
const sharedClient = `import socket, sys
def get(host, port):
    try:
        s = socket.create_connection((host, int(port)), timeout=10)
    except OSError as e:
        return type(e).__name__
    s.sendall(b"GET / HTTP/1.0\r\nHost: x\r\n\r\n")
    data = b""
    while True:
        b = s.recv(4096)
        if not b:
            break
        data += b
    return data.split(b"\r\n", 1)[0].decode()
for target in sys.argv[1:]:
    print(target, get(*target.rsplit(":", 1)))
`

// Containers of different stacks on one user-defined network reach one
// another by name, at ports with and without host ports, as containers on
// one Docker network do; a container on no network they share does not.
func TestSharedNetworks(t *testing.T) {
	const network = "e2e-shared"
	docker(t, "network", "rm", network)
	mustDocker(t, "network", "create", network)
	defer docker(t, "network", "rm", network)
	health := func(port string) []string {
		return []string{"--health-cmd", `python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:` + port + `')"`,
			"--health-interval", "1s", "--health-start-interval", "200ms", "--health-start-period", "30s"}
	}
	serve := func(name, port string, args ...string) {
		waitedRun(t, name, append(append(args, health(port)...), "fws/python:3.14", "python3", "-m", "http.server", port)...)
	}
	serve("e2e-shared-api", "8000", "--network", network, "--expose", "8000")
	serve("e2e-shared-cache", "8001", "--network", network, "--network-alias", "kv")
	serve("e2e-shared-loner", "8002", "--expose", "8002")

	out := mustDocker(t, "run", "--rm", "--network", network, "fws/python:3.14", "python3", "-c", sharedClient,
		"e2e-shared-api:8000", "kv:8001", "e2e-shared-api:9999", "e2e-shared-loner:8002")
	for _, want := range []string{
		"e2e-shared-api:8000 HTTP/1.0 200 OK\n", "kv:8001 HTTP/1.0 200 OK\n",
		"e2e-shared-api:9999 ConnectionRefusedError\n", "e2e-shared-loner:8002 gaierror\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("a client on %s printed %q, want %q in it", network, out, want)
		}
	}
	if out := mustDocker(t, "run", "--rm", "fws/python:3.14", "python3", "-c", sharedClient, "e2e-shared-api:8000", "kv:8001"); out != "e2e-shared-api:8000 gaierror\nkv:8001 gaierror\n" {
		t.Errorf("a client on no shared network printed %q", out)
	}
	// Datagrams cross too, to a port with a host port and one without, and
	// their replies come from the member's address and port.
	udp := "e2e-shared-udp"
	mustDocker(t, "run", "-d", "--name", udp, "--network", network, "--expose", "9054/udp", "fws/python:3.14", "python3", "-c", `import socket, threading
socks = []
for port in (9053, 9054):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.bind(("0.0.0.0", port))
    socks.append(s)
def echo(s):
    while True:
        d, a = s.recvfrom(512)
        s.sendto(d.upper(), a)
threads = [threading.Thread(target=echo, args=(s,)) for s in socks]
for th in threads:
    th.start()
print("ready", flush=True)
for th in threads:
    th.join()`)
	defer docker(t, "rm", "-f", udp)
	waitLog(t, udp, "ready")
	out = mustDocker(t, "run", "--rm", "--network", network, "fws/python:3.14", "python3", "-c", `import socket
addr = socket.gethostbyname("`+udp+`")
for port in (9053, 9054):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(10)
    s.sendto(b"ping", ("`+udp+`", port))
    d, a = s.recvfrom(512)
    print(port, d.decode(), "from-member" if a == (addr, port) else a)`)
	if out != "9053 PING from-member\n9054 PING from-member\n" {
		t.Errorf("a UDP client on %s printed %q", network, out)
	}
	// Disconnected, a container no longer reaches the network's members.
	mustDocker(t, "network", "disconnect", network, "e2e-shared-cache")
	if out := mustDocker(t, "exec", "e2e-shared-cache", "python3", "-c", sharedClient, "e2e-shared-api:8000"); out != "e2e-shared-api:8000 gaierror\n" {
		t.Errorf("a disconnected container printed %q", out)
	}
	mustDocker(t, "network", "connect", network, "e2e-shared-cache")
	if out := mustDocker(t, "exec", "e2e-shared-cache", "python3", "-c", sharedClient, "e2e-shared-api:8000"); out != "e2e-shared-api:8000 HTTP/1.0 200 OK\n" {
		t.Errorf("a reconnected container printed %q", out)
	}
}

// Two Compose projects on an external network reach each other's services
// on it by service name; a service not on it does not.
func TestComposeExternalNetwork(t *testing.T) {
	const network = "e2e-shared-compose"
	docker(t, "network", "rm", network)
	mustDocker(t, "network", "create", network)
	defer docker(t, "network", "rm", network)
	dir := t.TempDir()
	files := map[string]string{
		"a.yaml": `services:
  api:
    image: fws/python:3.14
    command: ["python3", "-m", "http.server", "8000"]
    healthcheck:
      test: ["CMD", "python3", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:8000')"]
      interval: 1s
      start_interval: 200ms
      start_period: 30s
    networks: [default, shared]
networks:
  shared:
    external: true
    name: ` + network + `
`,
		"b.yaml": `services:
  client:
    image: fws/python:3.14
    networks: [default, shared]
  loner:
    image: fws/python:3.14
networks:
  shared:
    external: true
    name: ` + network + `
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	composeFile := func(project, file string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker-compose", append([]string{"-p", project, "-f", filepath.Join(dir, file)}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker-compose -p %s %s: %v\n%s", project, strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	t.Cleanup(func() { composeFile("e2esharedb", "b.yaml", "down"); composeFile("e2eshareda", "a.yaml", "down") })
	composeFile("e2eshareda", "a.yaml", "up", "-d", "--wait")
	out := composeFile("e2esharedb", "b.yaml", "run", "--rm", "-T", "client", "python3", "-c", sharedClient, "api:8000")
	if !strings.Contains(out, "api:8000 HTTP/1.0 200 OK\n") {
		t.Errorf("project b's client on the external network printed %q", out)
	}
	out = composeFile("e2esharedb", "b.yaml", "run", "--rm", "-T", "loner", "python3", "-c", sharedClient, "api:8000")
	if !strings.Contains(out, "api:8000 gaierror\n") {
		t.Errorf("project b's service off the external network printed %q", out)
	}
}

// A legacy link on the default bridge network reaches a container of
// another stack, by the alias, the child's name and hostname, with Docker's
// variables, /etc/hosts entries and link names.
func TestLinks(t *testing.T) {
	const db, web = "e2e-link-db", "e2e-link-web"
	docker(t, "rm", "-f", web)
	waitedRun(t, db, "-e", "SECRET=s3", "--expose", "8000",
		"--health-cmd", `python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:8000')"`,
		"--health-interval", "1s", "--health-start-interval", "200ms", "--health-start-period", "30s",
		"fws/python:3.14", "python3", "-m", "http.server", "8000")
	client := sharedClient + `
import os
addr = socket.gethostbyname("database")
print(os.environ["DATABASE_PORT"] == "tcp://" + addr + ":8000", os.environ["DATABASE_NAME"], os.environ["DATABASE_ENV_SECRET"])
print([l.split()[1:] for l in open("/etc/hosts") if l.split()[:1] == [addr]])
`
	mustDocker(t, "run", "-d", "--name", web, "--link", db+":database", "fws/python:3.14", "python3", "-u", "-c", client+`
import time
time.sleep(600)`, "database:8000", db+":8000")
	defer docker(t, "rm", "-f", web)
	waitLog(t, web, "]]")
	out := mustDocker(t, "logs", web)
	hostname := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.Config.Hostname}}", db))
	for _, want := range []string{
		"database:8000 HTTP/1.0 200 OK\n", db + ":8000 HTTP/1.0 200 OK\n",
		"True /" + web + "/database s3\n",
		"[['database', '" + hostname + "', '" + db + "']]\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the linked client printed %q, want %q in it", out, want)
		}
	}
	if out := mustDocker(t, "run", "--rm", "fws/python:3.14", "python3", "-c", sharedClient, db+":8000", "database:8000"); out != db+":8000 gaierror\ndatabase:8000 gaierror\n" {
		t.Errorf("an unlinked client printed %q", out)
	}
	if got := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{json .HostConfig.Links}}", web)); got != `["/`+db+`:/`+web+`/database"]` {
		t.Errorf("inspect reports the links as %s", got)
	}
	if got := strings.TrimSpace(mustDocker(t, "ps", "--filter", "name=^/"+db+"$", "--format", "{{.Names}}")); got != db+","+web+"/database" {
		t.Errorf("ps names the child %q", got)
	}
	mustDocker(t, "rm", "--link", web+"/database")
	if got := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{json .HostConfig.Links}}", web)); got != "null" {
		t.Errorf("after rm --link, inspect reports %s", got)
	}
	if _, err := docker(t, "rm", "--link", db); err == nil || !strings.Contains(err.Error(), "cannot remove the default link name") {
		t.Errorf("rm --link of a container's own name: %v", err)
	}
	if _, err := docker(t, "run", "--rm", "--link", "e2e-link-nothing", "fws/python:3.14", "true"); err == nil || !strings.Contains(err.Error(), "could not get container for e2e-link-nothing") {
		t.Errorf("a link to no container: %v", err)
	}
}

// A container resolves the names its /etc/hosts lists, its own hostname to
// its member address, as Docker writes them.
func TestHostsResolve(t *testing.T) {
	out := mustDocker(t, "run", "--rm", "--hostname", "e2e-hosted", "--add-host", "extra.local:10.1.2.3", "--add-host", "host.docker.internal:host-gateway",
		"fws/python:3.14", "python3", "-c", `import os, socket
print(socket.gethostbyname("extra.local"), socket.gethostbyname("host.docker.internal"), socket.gethostbyname("e2e-hosted").startswith("127.88."), os.environ["HOSTNAME"])`)
	if out != "10.1.2.3 127.0.0.1 True e2e-hosted\n" {
		t.Errorf("the container resolved %q", out)
	}
}

// Compose's links give a service an alias of its own for another on the
// project's network.
func TestComposeLinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(`services:
  db:
    image: fws/python:3.14
    command: ["python3", "-m", "http.server", "8000"]
    healthcheck:
      test: ["CMD", "python3", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:8000')"]
      interval: 1s
      start_interval: 200ms
      start_period: 30s
  client:
    image: fws/python:3.14
    command: ["python3", "-c", "import time; time.sleep(600)"]
    links: ["db:database"]
    depends_on:
      db: {condition: service_healthy}
  other:
    image: fws/python:3.14
    command: ["python3", "-c", "import time; time.sleep(600)"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker-compose", append([]string{"-p", "e2e-links", "-f", filepath.Join(dir, "compose.yaml")}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker-compose %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	defer run("down", "-t", "0")
	run("up", "-d", "--wait")
	if out := run("exec", "-T", "client", "python3", "-c", sharedClient, "database:8000", "db:8000"); out != "database:8000 HTTP/1.0 200 OK\ndb:8000 HTTP/1.0 200 OK\n" {
		t.Errorf("the linking service printed %q", out)
	}
	if out := run("exec", "-T", "other", "python3", "-c", sharedClient, "database:8000", "db:8000"); out != "database:8000 gaierror\ndb:8000 HTTP/1.0 200 OK\n" {
		t.Errorf("another service printed %q", out)
	}
}
