//go:build e2e && darwin

// Package e2e drives fwsd with the real Docker CLI and Docker Compose: two
// Compose stacks run side by side, and from inside one the tests try to reach
// the other and the host.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	dockerHost string
	stateRoot  string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	images := os.Getenv("FWS_E2E_IMAGES")
	if images == "" {
		fmt.Fprintln(os.Stderr, "FWS_E2E_IMAGES must name the directory scripts/build-images.sh wrote")
		return 1
	}
	for _, tool := range []string{"docker", "docker-compose"} {
		if _, err := exec.LookPath(tool); err != nil {
			fmt.Fprintf(os.Stderr, "%s is required: %v\n", tool, err)
			return 1
		}
	}
	home, _ := os.UserHomeDir()
	// Unix socket paths are capped at 104 bytes on macOS, and Postgres puts
	// its socket under the container root, so the state root stays short.
	root, err := os.MkdirTemp(home, ".fws-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(root)
	stateRoot, _ = filepath.EvalSymlinks(root)

	bin := filepath.Join(root, "fwsd")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/fwsd").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fwsd: %v\n%s", err, out)
		return 1
	}
	sock := filepath.Join(root, "docker.sock")
	daemon := exec.Command(bin, "-root", filepath.Join(root, "state"), "-socket", sock, "-port-base", "30000", "-port-limit", "31000", "-block-size", "50")
	stateRoot = filepath.Join(stateRoot, "state")
	stderr, _ := daemon.StderrPipe()
	if err := daemon.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		daemon.Process.Signal(syscall.SIGTERM)
		daemon.Wait()
	}()
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stderr)
		announced := false
		for sc.Scan() {
			fmt.Fprintln(os.Stderr, "fwsd:", sc.Text())
			if !announced && strings.Contains(sc.Text(), "serving the Docker API") {
				announced = true
				close(ready)
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		fmt.Fprintln(os.Stderr, "fwsd did not start")
		return 1
	}
	dockerHost = "unix://" + sock
	os.Setenv("DOCKER_HOST", dockerHost)

	tars, _ := filepath.Glob(filepath.Join(images, "*.tar"))
	if len(tars) == 0 {
		fmt.Fprintf(os.Stderr, "no image archives in %s\n", images)
		return 1
	}
	for _, t := range tars {
		if out, err := exec.Command("docker", "load", "-i", t).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "docker load %s: %v\n%s", t, err, out)
			return 1
		}
	}
	return m.Run()
}

func docker(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("docker", args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

func mustDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := docker(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func compose(project string, args ...string) (string, error) {
	cmd := exec.Command("docker-compose", append([]string{"-p", project, "-f", "testdata/compose.yaml"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker-compose -p %s %s: %v\n%s", project, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func mustCompose(t *testing.T, project string, args ...string) string {
	t.Helper()
	out, err := compose(project, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDockerRun(t *testing.T) {
	out := mustDocker(t, "run", "--rm", "fws/toolbox:1", "echo", "hello", "from", "a", "sandbox")
	if strings.TrimSpace(out) != "hello from a sandbox" {
		t.Fatalf("docker run output %q", out)
	}
	if _, err := docker(t, "run", "--rm", "--memory", "64m", "fws/toolbox:1", "echo"); err == nil || !strings.Contains(err.Error(), "memory limits are not supported") {
		t.Fatalf("memory limit: %v", err)
	}
	if _, err := docker(t, "run", "--rm", "--network", "host", "fws/toolbox:1", "echo"); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("host network: %v", err)
	}
	if out := mustDocker(t, "ps", "-a", "-q"); strings.TrimSpace(out) != "" {
		t.Fatalf("--rm left containers behind: %q", out)
	}
}

type stack struct {
	name   string
	env    map[string]string // the app container's SANDBOX_* variables
	dbPort string            // published host port of db
	webURL string
	dbPid  int
	dbRoot string
	dbVol  string
}

func publishedPort(t *testing.T, project, service, port string) string {
	t.Helper()
	out := strings.TrimSpace(mustCompose(t, project, "port", service, port))
	_, p, err := net.SplitHostPort(out)
	if err != nil {
		t.Fatalf("compose port %s %s: %q", service, port, out)
	}
	return p
}

type inspect struct {
	State struct {
		Pid int
	}
	GraphDriver struct {
		Data map[string]string
	}
	Mounts []struct {
		Type, Name, Source, Destination string
	}
}

func inspectService(t *testing.T, project, service string) inspect {
	t.Helper()
	id := strings.TrimSpace(mustCompose(t, project, "ps", "-q", service))
	var res []inspect
	if err := json.Unmarshal([]byte(mustDocker(t, "inspect", id)), &res); err != nil || len(res) != 1 {
		t.Fatalf("inspect %s/%s: %v", project, service, err)
	}
	return res[0]
}

func describe(t *testing.T, project string) stack {
	t.Helper()
	s := stack{name: project, env: map[string]string{}}
	for _, l := range strings.Split(mustCompose(t, project, "exec", "-T", "app", "/toolbox", "env"), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			s.env[k] = v
		}
	}
	s.dbPort = publishedPort(t, project, "db", "5432")
	s.webURL = "http://127.0.0.1:" + publishedPort(t, project, "app", "8080")
	db := inspectService(t, project, "db")
	s.dbPid, s.dbRoot = db.State.Pid, db.GraphDriver.Data["MergedDir"]
	for _, m := range db.Mounts {
		if m.Destination == "/var/lib/postgresql/data" {
			s.dbVol = m.Source
		}
	}
	if s.dbPid == 0 || s.dbRoot == "" || s.dbVol == "" || s.env["SANDBOX_DB_PORT_5432"] == "" {
		t.Fatalf("stack %s incompletely described: %+v", project, s)
	}
	return s
}

func query(t *testing.T, port, sql string, args ...any) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, "postgres://postgres:secret@127.0.0.1:"+port+"/app?sslmode=disable")
	if err != nil {
		t.Fatalf("connect to postgres on %s: %v", port, err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func counter(t *testing.T, s stack) (string, int) {
	t.Helper()
	res, err := http.Get(s.webURL + "/counter")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("%s/counter: %s: %s", s.webURL, res.Status, b)
	}
	var v struct {
		Stack string
		Hits  int
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("counter body %q: %v", b, err)
	}
	return v.Stack, v.Hits
}

// probe runs a toolbox operation inside the project's app container.
func probe(project, op, arg string) error {
	_, err := compose(project, "exec", "-T", "app", "/toolbox", op, arg)
	return err
}

func TestTwoStacksRunSideBySideInIsolation(t *testing.T) {
	projects := []string{"alpha", "beta"}
	for _, p := range projects {
		t.Cleanup(func() { compose(p, "down", "-v") })
	}
	var wg sync.WaitGroup
	errs := make([]error, len(projects))
	for i, p := range projects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = compose(p, "up", "-d", "--wait")
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	a, b := describe(t, "alpha"), describe(t, "beta")

	t.Run("each stack serves its own data", func(t *testing.T) {
		for _, s := range []stack{a, b} {
			query(t, s.dbPort, "CREATE TABLE owner (name text)")
			query(t, s.dbPort, "INSERT INTO owner VALUES ($1) RETURNING name", s.name)
		}
		for _, s := range []stack{a, b} {
			if got := query(t, s.dbPort, "SELECT name FROM owner"); len(got) != 1 || got[0] != s.name {
				t.Errorf("stack %s database holds %v", s.name, got)
			}
		}
		for i := 1; i <= 2; i++ {
			if stk, hits := counter(t, a); stk != "alpha" || hits != i {
				t.Errorf("alpha counter = %s/%d, want alpha/%d", stk, hits, i)
			}
		}
		if stk, hits := counter(t, b); stk != "beta" || hits != 1 {
			t.Errorf("beta counter = %s/%d, want beta/1: the stacks share a cache", stk, hits)
		}
	})

	t.Run("same service ports land on different host ports", func(t *testing.T) {
		if a.env["SANDBOX_DB_PORT_5432"] == b.env["SANDBOX_DB_PORT_5432"] || a.dbPort == b.dbPort {
			t.Fatalf("db ports clash: alpha %s/%s beta %s/%s", a.env["SANDBOX_DB_PORT_5432"], a.dbPort, b.env["SANDBOX_DB_PORT_5432"], b.dbPort)
		}
	})

	t.Run("network", func(t *testing.T) {
		if err := probe("alpha", "connect", a.env["SANDBOX_DB_PORT_5432"]); err != nil {
			t.Errorf("alpha app cannot reach its own db: %v", err)
		}
		for _, port := range []string{b.env["SANDBOX_DB_PORT_5432"], b.env["SANDBOX_CACHE_PORT_6379"], b.env["SANDBOX_PORT_8080"]} {
			if err := probe("alpha", "connect", port); err == nil {
				t.Errorf("alpha app reached beta's port %s", port)
			}
		}
		if err := probe("alpha", "connect", b.dbPort); err == nil {
			t.Errorf("alpha app reached beta's db through its published port %s", b.dbPort)
		}
	})

	t.Run("filesystem", func(t *testing.T) {
		home, _ := os.UserHomeDir()
		root := a.env["SANDBOX_ROOT"]
		if err := probe("alpha", "list", root); err != nil {
			t.Errorf("alpha app cannot list its own root: %v", err)
		}
		if err := probe("alpha", "write", filepath.Join(root, "tmp", "probe")); err != nil {
			t.Errorf("alpha app cannot write its own root: %v", err)
		}
		for _, c := range []struct{ op, path string }{
			{"list", b.dbRoot},
			{"list", b.dbVol},
			{"read", filepath.Join(b.dbVol, "PG_VERSION")},
			{"write", filepath.Join(b.dbRoot, "tmp", "planted")},
			{"list", home},
			{"list", "/nix/store"},
			{"write", "/private/tmp/fws-escape"},
			{"read", filepath.Join(stateRoot, "alloc.json")},
		} {
			if err := probe("alpha", c.op, c.path); err == nil {
				t.Errorf("alpha app could %s %s", c.op, c.path)
			}
		}
	})

	t.Run("processes", func(t *testing.T) {
		if err := probe("alpha", "signal", fmt.Sprint(b.dbPid)); err == nil {
			t.Errorf("alpha app could signal beta's postgres (pid %d)", b.dbPid)
		}
		if err := probe("alpha", "exec", "/bin/ls"); err == nil {
			t.Errorf("alpha app could exec a host binary")
		}
	})

	t.Run("a taken host port fails like Docker", func(t *testing.T) {
		_, err := docker(t, "run", "--rm", "-p", a.dbPort+":8080", "fws/toolbox:1")
		if err == nil || !strings.Contains(err.Error(), "port is already allocated") {
			t.Fatalf("publishing alpha's port again: %v", err)
		}
	})

	t.Run("logs", func(t *testing.T) {
		if out := mustCompose(t, "alpha", "logs", "db"); !strings.Contains(out, "database system is ready to accept connections") {
			t.Fatalf("db logs lack the ready line:\n%s", out)
		}
	})

	t.Run("tearing one stack down leaves the other running", func(t *testing.T) {
		mustCompose(t, "alpha", "down", "-v")
		if got := query(t, b.dbPort, "SELECT name FROM owner"); len(got) != 1 || got[0] != "beta" {
			t.Fatalf("beta after alpha's teardown: %v", got)
		}
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+a.dbPort, time.Second); err == nil {
			c.Close()
			t.Errorf("alpha's published db port %s still accepts connections", a.dbPort)
		}
		if _, err := os.Stat(filepath.Join(stateRoot, "stacks", "alpha")); !os.IsNotExist(err) {
			t.Errorf("alpha's stack directory survived down: %v", err)
		}
		if _, err := os.Stat(a.dbVol); !os.IsNotExist(err) {
			t.Errorf("alpha's volume survived down -v: %v", err)
		}
	})
}
