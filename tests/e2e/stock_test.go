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
	}{
		{
			name: "python", image: "fws/python:3.14", port: "8000", body: "Directory listing",
			health: `python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:8000')"`,
			cmd:    []string{"python3", "-m", "http.server", "8000"},
		},
		{
			name: "node", image: "fws/node:26", port: "3000", body: "from node",
			health: `node -e "fetch('http://localhost:3000').then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))"`,
			cmd:    []string{"node", "-e", "require('http').createServer((q, r) => r.end('from node')).listen(3000)"},
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
		})
	}
}
