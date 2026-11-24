//go:build e2e && darwin

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Each of Testcontainers' wait strategies that its users and modules
// lean on holds a container back until what it waits for is so: a command
// in the container answers, the image's health check passes, an HTTP
// endpoint answers with the body it should, a file appears, a database
// takes a query, and all of several strategies at once.
func TestTestcontainersWaitStrategies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run := func(t *testing.T, img string, opts ...testcontainers.ContainerCustomizer) testcontainers.Container {
		t.Helper()
		c, err := testcontainers.Run(ctx, img, opts...)
		testcontainers.CleanupContainer(t, c)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Run("exec", func(t *testing.T) {
		run(t, "fws/redis:8", testcontainers.WithExposedPorts("6379/tcp"),
			testcontainers.WithWaitStrategy(wait.ForExec([]string{"redis-cli", "ping"}).
				WithResponseMatcher(func(body io.Reader) bool {
					b, _ := io.ReadAll(body)
					return bytes.Contains(b, []byte("PONG"))
				})))
	})
	t.Run("health", func(t *testing.T) {
		c := run(t, "fws/toolbox:1", testcontainers.WithExposedPorts("8080/tcp"),
			testcontainers.WithWaitStrategy(wait.ForHealthCheck()))
		st, err := c.State(ctx)
		if err != nil || st.Health == nil || st.Health.Status != "healthy" {
			t.Fatalf("the container's state after the health wait: %+v, %v", st, err)
		}
	})
	t.Run("http", func(t *testing.T) {
		c := run(t, "fws/toolbox:1", testcontainers.WithExposedPorts("8080/tcp"),
			testcontainers.WithWaitStrategy(wait.ForHTTP("/healthz").WithPort("8080/tcp").
				WithStatusCodeMatcher(func(code int) bool { return code == 200 }).
				WithResponseMatcher(func(body io.Reader) bool {
					b, _ := io.ReadAll(body)
					return strings.TrimSpace(string(b)) == "ok"
				})))
		endpoint, err := c.PortEndpoint(ctx, "8080/tcp", "http")
		if err != nil {
			t.Fatal(err)
		}
		if body := httpGet(t, endpoint+"/healthz"); strings.TrimSpace(body) != "ok" {
			t.Fatalf("/healthz after the HTTP wait: %q", body)
		}
	})
	t.Run("file", func(t *testing.T) {
		run(t, "fws/busybox:1", testcontainers.WithCmd("sh", "-c", "sleep 2; echo done > /tmp/marker; sleep 600"),
			testcontainers.WithWaitStrategy(wait.ForFile("/tmp/marker").WithMatcher(func(r io.Reader) error {
				b, _ := io.ReadAll(r)
				if strings.TrimSpace(string(b)) != "done" {
					return fmt.Errorf("the marker reads %q", b)
				}
				return nil
			})))
	})
	t.Run("sql", func(t *testing.T) {
		run(t, "fws/postgres:16", testcontainers.WithExposedPorts("5432/tcp"),
			testcontainers.WithEnv(map[string]string{"POSTGRES_PASSWORD": "secret"}),
			testcontainers.WithWaitStrategy(wait.ForSQL("5432/tcp", "pgx", func(host string, port network.Port) string {
				return fmt.Sprintf("postgres://postgres:secret@%s:%s/postgres?sslmode=disable", host, port.Port())
			}).WithStartupTimeout(2*time.Minute)))
	})
	t.Run("all", func(t *testing.T) {
		run(t, "fws/redis:8", testcontainers.WithExposedPorts("6379/tcp"),
			testcontainers.WithWaitStrategy(wait.ForAll(
				wait.ForLog("Ready to accept connections"),
				wait.ForListeningPort("6379/tcp"),
				wait.ForMappedPort("6379/tcp"),
			)))
	})
}

// logLines is a Testcontainers log consumer that keeps what it is given.
type logLines struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logLines) Accept(entry testcontainers.Log) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Write(entry.Content)
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// What Testcontainers does with a running container: copies a file and a
// directory into it and reads them back, follows its log through a log
// consumer, stops and starts it, after which its port answers again, and
// reuses a container of the same name rather than creating a second.
func TestTestcontainersLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	consumer := &logLines{}
	c, err := testcontainers.Run(ctx, "fws/toolbox:1",
		testcontainers.WithExposedPorts("8080/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/healthz").WithPort("8080/tcp")),
		testcontainers.WithLogConsumers(consumer))
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	cat := func(path string) string {
		t.Helper()
		code, r, err := c.Exec(ctx, []string{"/toolbox", "cat", path}, tcexec.Multiplexed())
		b, _ := io.ReadAll(r)
		if err != nil || code != 0 {
			t.Fatalf("cat %s: code %d, %v\n%s", path, code, err, b)
		}
		return string(b)
	}
	if err := c.CopyToContainer(ctx, []byte("copied in\n"), "/tmp/copied.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cat("/tmp/copied.txt"); got != "copied in\n" {
		t.Errorf("a file copied into the running container reads %q", got)
	}
	dir := filepath.Join(t.TempDir(), "tree")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "leaf.txt"), []byte("leaf\n"), 0o644)
	// The path names the directory as it lands: testcontainers-go unpacks
	// it into the path's parent, under its own name.
	if err := c.CopyDirToContainer(ctx, dir, "/tmp/tree", 0o755); err != nil {
		t.Fatal(err)
	}
	if got := cat("/tmp/tree/sub/leaf.txt"); got != "leaf\n" {
		t.Errorf("a file of a directory copied in reads %q", got)
	}
	rc, err := c.CopyFileFromContainer(ctx, "/tmp/tree/sub/leaf.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "leaf\n" {
		t.Errorf("the file copied back out reads %q", b)
	}
	if !strings.Contains(consumer.String(), "toolbox listening on") {
		t.Errorf("the log consumer was given %q", consumer.String())
	}

	timeout := 10 * time.Second
	if err := c.Stop(ctx, &timeout); err != nil {
		t.Fatal(err)
	}
	if st, err := c.State(ctx); err != nil || st.Running {
		t.Fatalf("the stopped container's state: %+v, %v", st, err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("starting the stopped container, which waits for its HTTP endpoint again: %v", err)
	}
	endpoint, err := c.PortEndpoint(ctx, "8080/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	if body := httpGet(t, endpoint+"/healthz"); strings.TrimSpace(body) != "ok" {
		t.Errorf("/healthz after the restart: %q", body)
	}

	const name = "fws-e2e-tc-reuse"
	first, err := testcontainers.Run(ctx, "fws/redis:8", testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")), testcontainers.WithReuseByName(name))
	testcontainers.CleanupContainer(t, first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := testcontainers.Run(ctx, "fws/redis:8", testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")), testcontainers.WithReuseByName(name))
	if err != nil {
		t.Fatal(err)
	}
	if first.GetContainerID() != second.GetContainerID() {
		testcontainers.CleanupContainer(t, second)
		t.Errorf("reuse by name made a second container, %s beside %s", second.GetContainerID(), first.GetContainerID())
	}
}
