//go:build e2e && darwin

package e2e

import (
	"io"
	"net/http"
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
	waitedRun(t, "fws-e2e-unexposed-web", append(append(labels("web"),
		"--health-cmd", `python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:8002')"`,
		"--health-interval", "1s", "--health-start-interval", "200ms", "--health-start-period", "30s",
		"fws/python:3.14"), "python3", "-m", "http.server", "8002")...)
	client := `import socket, urllib.request
print(urllib.request.urlopen("http://web:8002").status)
s = socket.create_connection(("web", 8002), timeout=5)
print(s.getpeername())
try:
    socket.create_connection(("web", 8003), timeout=5)
    print("closed port open")
except ConnectionRefusedError:
    print("closed port refused")
`
	out := mustDocker(t, append(append([]string{"run", "--rm"}, labels("client")...), "fws/python:3.14", "python3", "-c", client)...)
	for _, want := range []string{"200\n", "('127.88.", ", 8002)", "closed port refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("the client printed %q, want %q in it", out, want)
		}
	}
}
