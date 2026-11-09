//go:build e2e && darwin

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	tcinfluxdb "github.com/testcontainers/testcontainers-go/modules/influxdb"
	tcopensearch "github.com/testcontainers/testcontainers-go/modules/opensearch"
	tctoxiproxy "github.com/testcontainers/testcontainers-go/modules/toxiproxy"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// execOut runs a command in c and returns its output, failing the test
// when it does not succeed.
func execOut(ctx context.Context, t *testing.T, c testcontainers.Container, cmd ...string) string {
	t.Helper()
	code, r, err := c.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("%q: %v", cmd, err)
	}
	b, _ := io.ReadAll(r)
	if code != 0 {
		t.Fatalf("%q: exit %d: %s", cmd, code, b)
	}
	return string(b)
}

// ClickHouse creates the module's user and database through its image's
// entrypoint, runs an init script, and answers over HTTP and, through
// clickhouse-client in the container, its native protocol.
func TestClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	script := filepath.Join(t.TempDir(), "notes.sql")
	if err := os.WriteFile(script, []byte("CREATE TABLE analytics.notes (id UInt32, text String) ENGINE = MergeTree ORDER BY id;\n"+
		"INSERT INTO analytics.notes VALUES (1, 'kept');\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := tcclickhouse.Run(ctx, "fws/clickhouse:26.5", tcclickhouse.WithUsername("reporter"), tcclickhouse.WithPassword("s3cret"),
		tcclickhouse.WithDatabase("analytics"), tcclickhouse.WithInitScripts(script))
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		if c != nil {
			serverLogs(t, c)
		}
		t.Fatal(err)
	}
	logsOnFailure(t, c)
	base, err := c.PortEndpoint(ctx, "8123/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	query := "SELECT text FROM analytics.notes WHERE id = 1"
	if code, _ := httpDo(t, "POST", base+"/", query, map[string]string{"X-ClickHouse-User": "reporter", "X-ClickHouse-Key": "wrong"}); code == 200 {
		t.Errorf("a wrong password: %d", code)
	}
	if code, body := httpDo(t, "POST", base+"/", query, map[string]string{"X-ClickHouse-User": "reporter", "X-ClickHouse-Key": "s3cret"}); code != 200 || body != "kept\n" {
		t.Errorf("the query over HTTP: %d %q", code, body)
	}
	if out := execOut(ctx, t, c, "clickhouse-client", "--user", "reporter", "--password", "s3cret", "-q", "SELECT count() FROM analytics.notes"); strings.TrimSpace(out) != "1" {
		t.Errorf("the query over the native protocol: %q", out)
	}
}

// InfluxDB sets up its organization, bucket and admin token through the
// image's entrypoint, takes line protocol and answers Flux, and leaves the
// influx CLI configured for the server's final port.
func TestInfluxDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := tcinfluxdb.Run(ctx, "fws/influxdb:2.7", tcinfluxdb.WithV2Auth("acme", "metrics", "admin", "password123"),
		tcinfluxdb.WithV2AdminToken("a-token"))
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	logsOnFailure(t, c)
	base, err := c.ConnectionUrl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := httpDo(t, "GET", base+"/api/v2/buckets", "", nil); code != 401 {
		t.Errorf("a request without the token: %d", code)
	}
	auth := map[string]string{"Authorization": "Token a-token"}
	if code, body := httpDo(t, "POST", base+"/api/v2/write?org=acme&bucket=metrics&precision=s", "cpu,host=a usage=42i 1700000000", auth); code != 204 {
		t.Fatalf("write: %d %s", code, body)
	}
	flux := `from(bucket: "metrics") |> range(start: 0) |> filter(fn: (r) => r._measurement == "cpu")`
	code, body := httpDo(t, "POST", base+"/api/v2/query?org=acme", flux,
		map[string]string{"Authorization": "Token a-token", "Content-Type": "application/vnd.flux", "Accept": "application/csv"})
	if code != 200 || !strings.Contains(body, ",42,usage,cpu,a") {
		t.Fatalf("query: %d %s", code, body)
	}
	if out := execOut(ctx, t, c, "influx", "bucket", "list", "--hide-headers"); !strings.Contains(out, "\tmetrics\t") {
		t.Errorf("influx bucket list in the container: %q", out)
	}
}

// OpenSearch indexes a document and finds it.
func TestOpenSearch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := tcopensearch.Run(ctx, "fws/opensearch:3.5")
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	logsOnFailure(t, c)
	base, err := c.Address(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := map[string]string{"Content-Type": "application/json"}
	if code, body := httpDo(t, "GET", base+"/", "", h); code != 200 || !strings.Contains(body, `"number" : "3.5.0"`) {
		t.Errorf("the root: %d %s", code, body)
	}
	if code, body := httpDo(t, "PUT", base+"/notes/_doc/1?refresh=true", `{"text":"kept in the index"}`, h); code != 201 {
		t.Fatalf("index: %d %s", code, body)
	}
	code, body := httpDo(t, "GET", base+"/notes/_search?q=text:kept", "", h)
	var res struct {
		Hits struct {
			Hits []struct {
				ID string `json:"_id"`
			}
		}
	}
	if code != 200 || json.Unmarshal([]byte(body), &res) != nil || len(res.Hits.Hits) != 1 || res.Hits.Hits[0].ID != "1" {
		t.Fatalf("search: %d %s", code, body)
	}
}

// Toxiproxy proxies a stack member it reaches by name, and a latency toxic
// added through its API delays the proxied responses.
func TestToxiproxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	nw, err := tcnetwork.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer nw.Remove(context.Background())
	web, err := testcontainers.Run(ctx, "fws/nginx:1", testcontainers.WithExposedPorts("80/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("80/tcp")), tcnetwork.WithNetwork([]string{"web"}, nw))
	testcontainers.CleanupContainer(t, web)
	if err != nil {
		t.Fatal(err)
	}
	c, err := tctoxiproxy.Run(ctx, "fws/toxiproxy:2.12", tctoxiproxy.WithProxy("web", "web:80"), tcnetwork.WithNetwork(nil, nw))
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	logsOnFailure(t, c)
	host, port, err := c.ProxiedEndpoint(8666)
	if err != nil {
		t.Fatal(err)
	}
	proxied := fmt.Sprintf("http://%s:%s/", host, port)
	if code, body := httpDo(t, "GET", proxied, "", nil); code != 200 || !strings.Contains(body, "nginx") {
		t.Fatalf("through the proxy: %d %.200s", code, body)
	}
	api, err := c.URI(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := httpDo(t, "POST", api+"/proxies/web/toxics", `{"type":"latency","stream":"downstream","attributes":{"latency":700}}`,
		map[string]string{"Content-Type": "application/json"}); code != 200 {
		t.Fatalf("add a latency toxic: %d %s", code, body)
	}
	start := time.Now()
	res, err := http.Get(proxied)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if took := time.Since(start); took < 700*time.Millisecond {
		t.Errorf("a response through the latency toxic took %v", took)
	}
}

// serverLogs reports the processes of a ClickHouse container that never
// became ready and the end of its server's own logs.
func serverLogs(t *testing.T, c testcontainers.Container) {
	t.Helper()
	out, err := docker(t, "top", c.GetContainerID())
	if err == nil {
		t.Logf("the container's processes:\n%s", out)
		// Each thread's stack, which shows what a server that ignores
		// SIGTERM waits on.
		for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
			if f := strings.Fields(line); len(f) > 1 {
				s, _ := exec.Command("/usr/bin/sample", f[1], "1").CombinedOutput()
				if len(s) > 64<<10 {
					s = s[:64<<10]
				}
				t.Logf("sample %s:\n%s", f[1], s)
			}
		}
	}
	for _, f := range []string{"/var/log/clickhouse-server/clickhouse-server.err.log", "/var/log/clickhouse-server/clickhouse-server.log"} {
		r, err := c.CopyFileFromContainer(context.Background(), f)
		if err != nil {
			t.Logf("%s: %v", f, err)
			continue
		}
		b, _ := io.ReadAll(r)
		r.Close()
		var kept []string
		for _, l := range strings.Split(string(b), "\n") {
			if !strings.Contains(l, "<Trace>") && !strings.Contains(l, "<Debug>") && !strings.Contains(l, "<Test>") {
				kept = append(kept, l)
			}
		}
		if len(kept) > 200 {
			kept = kept[len(kept)-200:]
		}
		t.Logf("%s, above Debug:\n%s", f, strings.Join(kept, "\n"))
	}
}
