//go:build e2e && darwin

package e2e

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// hostAddress returns a non-loopback IPv4 address of this machine.
func hostAddress(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	t.Fatal("this machine has no non-loopback IPv4 address")
	return ""
}

func TestEgressReachesTheInternetButNotTheHost(t *testing.T) {
	// A service on the host's own network address: reachable from the
	// internet's point of view, but it is the host.
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "host data") })}
	go srv.Serve(l)
	defer srv.Close()
	hostURL := fmt.Sprintf("http://%s:%d/", hostAddress(t), l.Addr().(*net.TCPAddr).Port)
	if res, err := http.Get(hostURL); err != nil {
		t.Fatalf("the host service is not reachable from the host itself: %v", err)
	} else {
		res.Body.Close()
	}

	run := func(args ...string) error {
		_, err := docker(t, append([]string{"run", "--rm"}, args...)...)
		return err
	}
	if err := run("fws/toolbox:1", "fetch", "https://github.com/"); err != nil {
		t.Errorf("container could not reach the internet through its proxy: %v", err)
	}
	if err := run("fws/toolbox:1", "fetch", hostURL); err == nil {
		t.Errorf("container reached the host at %s through the proxy", hostURL)
	}
	if err := run("fws/toolbox:1", "dial", "github.com:443"); err == nil {
		t.Error("container dialled the internet directly, bypassing the proxy")
	}
	// Any TCP leaves through the proxy's SOCKS5 side, which ALL_PROXY names,
	// and is refused on the host as HTTP is.
	if err := run("fws/toolbox:1", "tcp", "github.com:443"); err != nil {
		t.Errorf("container could not open TCP to the internet through SOCKS5: %v", err)
	}
	hostTCP := net.JoinHostPort(hostAddress(t), strconv.Itoa(l.Addr().(*net.TCPAddr).Port))
	if err := run("fws/toolbox:1", "tcp", hostTCP); err == nil {
		t.Errorf("container reached the host at %s through SOCKS5", hostTCP)
	}
	if err := run("--network", "none", "fws/toolbox:1", "fetch", "https://github.com/"); err == nil {
		t.Error("a container with network none reached the internet")
	}
	mustDocker(t, "network", "create", "--internal", "e2e-internal")
	defer docker(t, "network", "rm", "e2e-internal")
	if err := run("--network", "e2e-internal", "fws/toolbox:1", "fetch", "https://github.com/"); err == nil {
		t.Error("a container on an internal network reached the internet")
	}
}

func TestDockerCp(t *testing.T) {
	mustDocker(t, "run", "-d", "--name", "cptest", "fws/toolbox:1")
	defer docker(t, "rm", "-f", "cptest")
	dir := t.TempDir()
	src := filepath.Join(dir, "hello.txt")
	os.WriteFile(src, []byte("copied in\n"), 0o644)

	mustDocker(t, "cp", src, "cptest:/tmp/")
	if out := mustDocker(t, "exec", "cptest", "/toolbox", "cat", "/tmp/hello.txt"); out != "copied in\n" {
		t.Fatalf("file inside the container holds %q", out)
	}
	back := filepath.Join(dir, "back.txt")
	mustDocker(t, "cp", "cptest:/tmp/hello.txt", back)
	if b, _ := os.ReadFile(back); string(b) != "copied in\n" {
		t.Fatalf("copied back %q", b)
	}
	mustDocker(t, "cp", "cptest:/tmp", filepath.Join(dir, "tree"))
	if b, _ := os.ReadFile(filepath.Join(dir, "tree", "hello.txt")); string(b) != "copied in\n" {
		t.Fatalf("directory copy lacks the file: %q", b)
	}
	if _, err := docker(t, "cp", "cptest:/no/such/file", dir); err == nil || !strings.Contains(err.Error(), "Could not find the file") {
		t.Fatalf("copying a missing path: %v", err)
	}
}

func TestTestcontainers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	redis, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "fws/redis:8",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections"),
			Files: []testcontainers.ContainerFile{{
				Reader: strings.NewReader("seeded\n"), ContainerFilePath: "/tmp/seed.txt", FileMode: 0o644,
			}},
		},
		Started: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer redis.Terminate(context.Background())

	endpoint, err := redis.PortEndpoint(ctx, "6379/tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "PING\r\n")
	if line, _ := bufio.NewReader(c).ReadString('\n'); strings.TrimSpace(line) != "+PONG" {
		t.Fatalf("redis at %s answered %q", endpoint, line)
	}

	code, out, err := redis.Exec(ctx, []string{"/entry", "health"})
	if err != nil || code != 0 {
		b, _ := io.ReadAll(out)
		t.Fatalf("exec health: code %d, err %v, output %s", code, err, b)
	}
	rc, err := redis.CopyFileFromContainer(ctx, "/tmp/seed.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(b, []byte("seeded\n")) {
		t.Fatalf("file copied in at creation reads back %q", b)
	}
}

func TestTopAndStats(t *testing.T) {
	since := time.Now()
	mustDocker(t, "run", "-d", "--name", "toptest", "-p", "127.0.0.1::8080", "fws/toolbox:1")
	defer docker(t, "rm", "-f", "toptest")
	waitHealthy(t, "toptest", since)

	out := mustDocker(t, "top", "toptest")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 || !strings.Contains(lines[0], "PID") || !strings.Contains(out, "toolbox serve") {
		t.Fatalf("docker top:\n%s", out)
	}
	if strings.Contains(out, "fwsd") {
		t.Fatalf("docker top lists the engine's own process:\n%s", out)
	}

	out = mustDocker(t, "stats", "--no-stream", "--format", "{{.Name}}|{{.MemUsage}}|{{.PIDs}}|{{.CPUPerc}}", "toptest")
	f := strings.Split(strings.TrimSpace(out), "|")
	if len(f) != 4 || f[0] != "toptest" || f[2] != "1" || strings.HasPrefix(f[1], "0B") || !strings.HasSuffix(f[3], "%") {
		t.Fatalf("docker stats: %q", out)
	}

	// Health checks reach the server inside the container: like traffic
	// over a Docker container's loopback, none of it is network I/O.
	if out := strings.TrimSpace(mustDocker(t, "stats", "--no-stream", "--format", "{{.NetIO}}", "toptest")); out != "0B / 0B" {
		t.Fatalf("docker stats counts health checks as network I/O: %q", out)
	}
	addr := "http://" + strings.TrimSpace(mustDocker(t, "port", "toptest", "8080")) + "/healthz"
	for range 20 {
		httpGet(t, addr)
	}
	out = strings.TrimSpace(mustDocker(t, "stats", "--no-stream", "--format", "{{.NetIO}}", "toptest"))
	rx, tx, ok := strings.Cut(out, " / ")
	if !ok || rx == "0B" || tx == "0B" {
		t.Fatalf("docker stats network I/O after 20 requests through the published port: %q", out)
	}
}

// waitHealthy blocks until the container reports healthy, reading Docker's
// event stream from since so an event emitted before the call is not missed.
func waitHealthy(t *testing.T, name string, since time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	// A container that exits will never become healthy: its die event ends
	// the wait.
	cmd := exec.CommandContext(ctx, "docker", "events", "--since", since.Format(time.RFC3339Nano),
		"--filter", "container="+name, "--filter", "event=health_status", "--filter", "event=die", "--format", "{{.Action}}")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		cmd.Wait()
	}()
	sc := bufio.NewScanner(out)
	why := "the wait timed out"
	for sc.Scan() {
		ev := strings.TrimSpace(sc.Text())
		if ev == "health_status: healthy" {
			return
		}
		if ev == "die" {
			why = "the container exited"
			break
		}
	}
	// docker logs writes the container's standard error to its own.
	logs := exec.Command("docker", "logs", "--tail", "80", name)
	text, _ := logs.CombinedOutput()
	health, _ := docker(t, "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}} {{json .State.Health}}", name)
	t.Fatalf("%s never reported healthy: %s\nstate: %s\nlogs:\n%s%s", name, why, health, text, stacks(t, name))
}

// stacks samples where each of a running container's processes is, for a
// wait that timed out.
func stacks(t *testing.T, name string) string {
	t.Helper()
	out, err := docker(t, "top", name, "-o", "pid")
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, pid := range strings.Fields(out)[1:] {
		s, _ := exec.Command("sample", pid, "1", "-mayDie").CombinedOutput()
		lines := strings.Split(string(s), "\n")
		if len(lines) > 120 {
			lines = lines[:120]
		}
		fmt.Fprintf(&b, "\nprocess %s:\n%s\n", pid, strings.Join(lines, "\n"))
	}
	return b.String()
}

// startedAt is when the container last started, from which its events are
// those of its current run.
func startedAt(t *testing.T, name string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.StartedAt}}", name)))
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestPauseUnpauseUpdate(t *testing.T) {
	since := time.Now().Add(-time.Second)
	mustDocker(t, "run", "-d", "--name", "pausetest", "-p", "8080", "fws/toolbox:1")
	defer docker(t, "rm", "-f", "pausetest")
	waitHealthy(t, "pausetest", since)
	port := strings.TrimSpace(mustDocker(t, "port", "pausetest", "8080"))
	_, p, _ := net.SplitHostPort(port)
	get := func() error {
		c := http.Client{Timeout: 2 * time.Second}
		res, err := c.Get("http://127.0.0.1:" + p + "/healthz")
		if err == nil {
			res.Body.Close()
		}
		return err
	}
	if err := get(); err != nil {
		t.Fatalf("running container does not answer: %v", err)
	}
	mustDocker(t, "pause", "pausetest")
	if s := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.Status}} {{.State.Paused}}", "pausetest")); s != "paused true" {
		t.Fatalf("paused container reads %q", s)
	}
	if err := get(); err == nil {
		t.Fatal("a paused container answered a request")
	}
	if _, err := docker(t, "exec", "pausetest", "/toolbox", "echo"); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("exec into a paused container: %v", err)
	}
	mustDocker(t, "unpause", "pausetest")
	if err := get(); err != nil {
		t.Fatalf("unpaused container does not answer: %v", err)
	}

	mustDocker(t, "update", "--restart", "on-failure:3", "pausetest")
	if s := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.HostConfig.RestartPolicy.Name}}:{{.HostConfig.RestartPolicy.MaximumRetryCount}}", "pausetest")); s != "on-failure:3" {
		t.Fatalf("restart policy after update: %q", s)
	}
	mustDocker(t, "update", "--memory", "64m", "--memory-swap", "64m", "--cpus", "1.5", "--pids-limit", "40", "pausetest")
	if s := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.HostConfig.Memory}} {{.HostConfig.NanoCpus}} {{.HostConfig.PidsLimit}}", "pausetest")); s != "67108864 1500000000 40" {
		t.Fatalf("limits after update: %q", s)
	}
	if s := mustDocker(t, "stats", "--no-stream", "--format", "{{.MemUsage}}", "pausetest"); !strings.Contains(s, "/ 64MiB") {
		t.Fatalf("docker stats does not report the updated memory limit: %q", s)
	}
	if _, err := docker(t, "update", "--blkio-weight", "100", "pausetest"); err == nil || !strings.Contains(err.Error(), "block I/O weight of a running container") {
		t.Fatalf("block I/O weight update of a running container: %v", err)
	}
	mustDocker(t, "pause", "pausetest")
	mustDocker(t, "stop", "pausetest")
	if s := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.Status}}", "pausetest")); s != "exited" {
		t.Fatalf("stopping a paused container left it %q", s)
	}
}

func TestPullFromRegistry(t *testing.T) {
	reg := httptest.NewServer(registry.New())
	defer reg.Close()
	host := strings.TrimPrefix(reg.URL, "http://")

	tag, _ := name.NewTag("fws/toolbox:1")
	img, err := tarball.ImageFromPath(filepath.Join(os.Getenv("FWS_E2E_IMAGES"), "toolbox.tar"), &tag)
	if err != nil {
		t.Fatal(err)
	}
	pushed, _ := name.ParseReference(host + "/e2e/toolbox:pulled")
	if err := remote.Write(pushed, img); err != nil {
		t.Fatal(err)
	}
	mustDocker(t, "pull", pushed.String())
	defer docker(t, "rmi", pushed.String())
	if out := mustDocker(t, "run", "--rm", pushed.String(), "echo", "pulled"); strings.TrimSpace(out) != "pulled" {
		t.Fatalf("pulled image ran with output %q", out)
	}

	linux, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	linuxRef, _ := name.ParseReference(host + "/e2e/linux:1")
	if err := remote.Write(linuxRef, linux); err != nil {
		t.Fatal(err)
	}
	if _, err := docker(t, "pull", linuxRef.String()); err == nil || !strings.Contains(err.Error(), "does not match the engine platform") {
		t.Fatalf("pulling a non-darwin image: %v", err)
	}
}

func TestDockerBuild(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", `# a two-stage build of the toolbox
ARG BASE=fws/toolbox:1
FROM ${BASE} AS first
RUN ["/toolbox", "put", "/made/in-first", "from the first stage"]

FROM $BASE
ARG GREETING=hello
ENV GREETING=$GREETING \
    WHERE=/srv
LABEL purpose="e2e build"
WORKDIR $WHERE
COPY greeting.txt sub/ ./
COPY --from=first /made/in-first /etc/first.txt
RUN ["/toolbox", "put", "/srv/built.txt", "written by RUN"]
RUN ["/toolbox", "fetch", "https://github.com/"]
EXPOSE 9090
CMD ["cat", "/srv/built.txt"]
`)
	write("greeting.txt", "greetings from the context\n")
	write("sub/nested.txt", "nested\n")
	write("secret.txt", "must not be sent\n")
	write(".dockerignore", "secret.txt\n")

	build := func(args ...string) (string, error) {
		cmd := exec.Command("docker", append([]string{"build"}, append(args, dir)...)...)
		cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := build("-t", "e2e/built:1", "--build-arg", "GREETING=hi")
	if err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	defer docker(t, "rmi", "e2e/built:1")
	if !strings.Contains(out, "Step 1/") || !strings.Contains(out, "Successfully tagged e2e/built:1") {
		t.Errorf("build output:\n%s", out)
	}

	for args, want := range map[string]string{
		"":                      "written by RUN",
		"cat /srv/greeting.txt": "greetings from the context",
		"cat /srv/nested.txt":   "nested",
		"cat /etc/first.txt":    "from the first stage",
	} {
		run := []string{"run", "--rm", "e2e/built:1"}
		if args != "" {
			run = append(run, strings.Fields(args)...)
		}
		out := mustDocker(t, run...)
		if want != "" && strings.TrimSpace(out) != want {
			t.Errorf("docker run e2e/built:1 %s = %q, want %q", args, out, want)
		}
	}
	if _, err := docker(t, "run", "--rm", "e2e/built:1", "cat", "/srv/secret.txt"); err == nil {
		t.Error(".dockerignore did not keep secret.txt out of the image")
	}
	inspect := mustDocker(t, "image", "inspect", "-f", "{{.Os}} {{index .Config.Labels \"purpose\"}} {{.Config.WorkingDir}} {{range .Config.Env}}{{.}},{{end}}", "e2e/built:1")
	for _, want := range []string{"darwin ", "e2e build", "/srv", "GREETING=hi,", "WHERE=/srv,"} {
		if !strings.Contains(inspect, want) {
			t.Errorf("image inspect %q lacks %q", inspect, want)
		}
	}

	// WORKDIR (which makes /srv), two COPYs and two RUNs each add a layer.
	extendsBy(t, "fws/toolbox:1", "e2e/built:1", 5)

	write("Dockerfile", "FROM fws/toolbox:1\nRUN echo needs a shell\n")
	if out, err := build("-t", "e2e/noshell:1"); err == nil || !strings.Contains(out, "/bin/sh") {
		t.Errorf("shell-form RUN in an image without /bin/sh: %v\n%s", err, out)
	}
	write("Dockerfile", "FROM fws/toolbox:1\nRUN [\"/toolbox\", \"fetch\", \"https://github.com/\"]\n")
	if out, err := build("--network", "none", "-t", "e2e/offline:1"); err == nil {
		t.Errorf("a RUN step reached the internet with --network none:\n%s", out)
	}
}

func TestBuildKit(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", `FROM fws/toolbox:1 AS first
RUN ["/toolbox", "put", "/made/in-first", "from the first stage"]

FROM fws/toolbox:1
ARG GREETING=hello
ENV GREETING=$GREETING
WORKDIR /srv
COPY greeting.txt ./
COPY --from=first /made/in-first /etc/first.txt
RUN --mount=type=cache,target=/cache ["/toolbox", "put", "/cache/seen", "cached"]
RUN ["/toolbox", "put", "/srv/built.txt", "written by RUN"]
RUN ["/toolbox", "fetch", "https://github.com/"]
CMD ["cat", "/srv/built.txt"]
`)
	write("greeting.txt", "greetings from the context\n")
	write("compose.yaml", "services:\n  app:\n    build: .\n    image: e2e/composed:1\n")

	cli := func(name string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		return string(out), err
	}
	out, err := cli("docker", "buildx", "build", "--load", "-t", "e2e/bk:1", "--build-arg", "GREETING=hi", dir)
	if err != nil {
		t.Fatalf("docker buildx build: %v\n%s", err, out)
	}
	defer docker(t, "rmi", "e2e/bk:1")
	for args, want := range map[string]string{
		"":                      "written by RUN",
		"cat /srv/greeting.txt": "greetings from the context",
		"cat /etc/first.txt":    "from the first stage",
	} {
		run := []string{"run", "--rm", "e2e/bk:1"}
		if args != "" {
			run = append(run, strings.Fields(args)...)
		}
		if got := strings.TrimSpace(mustDocker(t, run...)); got != want {
			t.Errorf("docker run e2e/bk:1 %s = %q, want %q", args, got, want)
		}
	}
	if _, err := docker(t, "run", "--rm", "e2e/bk:1", "cat", "/cache/seen"); err == nil {
		t.Error("the cache mount's content ended up in the image")
	}
	if env := mustDocker(t, "image", "inspect", "-f", "{{.Os}} {{range .Config.Env}}{{.}},{{end}}", "e2e/bk:1"); !strings.Contains(env, "darwin ") || !strings.Contains(env, "GREETING=hi,") {
		t.Errorf("image inspect: %q", env)
	}

	extendsBy(t, "fws/toolbox:1", "e2e/bk:1", -1)

	out, err = cli("docker", "build", "-t", "e2e/bk:2", "--build-arg", "GREETING=hi", dir)
	if err != nil {
		t.Fatalf("docker build through BuildKit: %v\n%s", err, out)
	}
	defer docker(t, "rmi", "e2e/bk:2")
	if !strings.Contains(out, "CACHED") {
		t.Errorf("an identical rebuild used no cache:\n%s", out)
	}

	local := filepath.Join(t.TempDir(), "out")
	if out, err := cli("docker", "buildx", "build", "--output", "type=local,dest="+local, dir); err != nil {
		t.Fatalf("build to a local directory: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(local, "srv", "built.txt")); strings.TrimSpace(string(b)) != "written by RUN" {
		t.Errorf("local output holds %q", b)
	}

	if out, err := cli("docker-compose", "-f", filepath.Join(dir, "compose.yaml"), "build"); err != nil {
		t.Fatalf("docker compose build: %v\n%s", err, out)
	}
	defer docker(t, "rmi", "e2e/composed:1")
	if got := strings.TrimSpace(mustDocker(t, "run", "--rm", "e2e/composed:1")); got != "written by RUN" {
		t.Errorf("composed image ran with %q", got)
	}
}

// layersOf lists an image's layer diff IDs.
func layersOf(t *testing.T, ref string) []string {
	t.Helper()
	return strings.Fields(mustDocker(t, "image", "inspect", "-f", "{{range .RootFS.Layers}}{{.}} {{end}}", ref))
}

// extendsBy checks that image is an OCI image whose layers are base's
// followed by n more, or by at least one when n is negative.
func extendsBy(t *testing.T, base, image string, n int) {
	t.Helper()
	b, i := layersOf(t, base), layersOf(t, image)
	if len(b) == 0 || len(i) <= len(b) || (n >= 0 && len(i) != len(b)+n) {
		t.Fatalf("%s has %d layers on %s's %d, want %d more", image, len(i), base, len(b), n)
	}
	for k := range b {
		if b[k] != i[k] {
			t.Fatalf("%s's layer %d is %s, not %s's %s", image, k, i[k], base, b[k])
		}
	}
	if mt := strings.TrimSpace(mustDocker(t, "image", "inspect", "-f", "{{.Descriptor.MediaType}}", image)); mt != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("%s's manifest is %s, not an OCI image manifest", image, mt)
	}
}

func TestImageLifecycle(t *testing.T) {
	const box = "fws-e2e-commit"
	mustDocker(t, "run", "--name", box, "fws/toolbox:1", "put", "/greeting", "committed")
	defer docker(t, "rm", "-f", box)

	mustDocker(t, "commit", "--change", `CMD ["cat","/greeting"]`, "--change", "LABEL fws.e2e=commit", box, "fws-e2e/committed:1")
	defer docker(t, "rmi", "-f", "fws-e2e/committed:1")
	if out := mustDocker(t, "run", "--rm", "fws-e2e/committed:1"); strings.TrimSpace(out) != "committed" {
		t.Fatalf("the committed image ran with output %q", out)
	}
	if l := mustDocker(t, "image", "inspect", "-f", `{{index .Config.Labels "fws.e2e"}}`, "fws-e2e/committed:1"); strings.TrimSpace(l) != "commit" {
		t.Fatalf("the committed image's label is %q", l)
	}
	if h := mustDocker(t, "history", "--no-trunc", "fws-e2e/committed:1"); !strings.Contains(h, "put /greeting committed") {
		t.Fatalf("history does not show the committed command:\n%s", h)
	}
	extendsBy(t, "fws/toolbox:1", "fws-e2e/committed:1", 1)
	digest := strings.TrimSpace(mustDocker(t, "image", "inspect", "-f", "{{.Descriptor.Digest}}", "fws-e2e/committed:1"))

	dir := t.TempDir()
	exported := filepath.Join(dir, "export.tar")
	mustDocker(t, "export", "-o", exported, box)
	if !tarHas(t, exported, "greeting") {
		t.Fatal("the export does not hold /greeting")
	}

	saved := filepath.Join(dir, "save.tar")
	mustDocker(t, "save", "-o", saved, "fws-e2e/committed:1")
	for _, f := range []string{"oci-layout", "index.json", "manifest.json"} {
		if !tarHas(t, saved, f) {
			t.Fatalf("the saved archive lacks %s", f)
		}
	}
	id := strings.TrimSpace(mustDocker(t, "image", "inspect", "-f", "{{.ID}}", "fws-e2e/committed:1"))
	mustDocker(t, "rmi", "fws-e2e/committed:1")
	mustDocker(t, "load", "-i", saved)
	if again := strings.TrimSpace(mustDocker(t, "image", "inspect", "-f", "{{.ID}} {{.Descriptor.Digest}}", "fws-e2e/committed:1")); again != id+" "+digest {
		t.Fatalf("save and load changed the image from %s %s to %s", id, digest, again)
	}

	reg := httptest.NewServer(registry.New())
	defer reg.Close()
	pushed := strings.TrimPrefix(reg.URL, "http://") + "/e2e/committed:pushed"
	mustDocker(t, "tag", "fws-e2e/committed:1", pushed)
	if out := mustDocker(t, "push", pushed); !strings.Contains(out, "digest: "+digest) {
		t.Fatalf("push output, wanting digest %s:\n%s", digest, out)
	}
	mustDocker(t, "rmi", pushed)
	mustDocker(t, "pull", pushed)
	defer docker(t, "rmi", pushed)
	if out := mustDocker(t, "run", "--rm", pushed); strings.TrimSpace(out) != "committed" {
		t.Fatalf("the pushed and pulled image ran with output %q", out)
	}
	if again := strings.TrimSpace(mustDocker(t, "image", "inspect", "-f", "{{.ID}} {{.Descriptor.Digest}}", pushed)); again != id+" "+digest {
		t.Fatalf("push and pull changed the image from %s %s to %s", id, digest, again)
	}
}

func tarHas(t *testing.T, path, entry string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(h.Name, "./") == entry {
			return true
		}
	}
}

func TestDigestReferencesAndPrune(t *testing.T) {
	reg := httptest.NewServer(registry.New())
	defer reg.Close()
	repo := strings.TrimPrefix(reg.URL, "http://") + "/e2e/ref"
	mustDocker(t, "tag", "fws/toolbox:1", repo+":1")
	if out := mustDocker(t, "push", repo+":1"); !strings.Contains(out, "digest: sha256:") {
		t.Fatalf("push:\n%s", out)
	}
	mustDocker(t, "rmi", repo+":1")

	out := mustDocker(t, "pull", repo+":1")
	if !strings.Contains(out, "Already exists") || !strings.Contains(out, "Digest: sha256:") || !strings.Contains(out, "Downloaded newer image for "+repo+":1") {
		t.Fatalf("pulling an image whose layers the engine holds:\n%s", out)
	}
	if out := mustDocker(t, "pull", repo+":1"); !strings.Contains(out, "Image is up to date for "+repo+":1") {
		t.Fatalf("pulling again:\n%s", out)
	}
	digest := strings.TrimSpace(mustDocker(t, "images", "--digests", "--format", "{{.Digest}}", repo))
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("docker images --digests shows %q", digest)
	}
	if got := strings.TrimSpace(mustDocker(t, "run", "--rm", repo+"@"+digest, "echo", "by digest")); got != "by digest" {
		t.Fatalf("running by digest printed %q", got)
	}
	if out := mustDocker(t, "rmi", repo+":1"); !strings.Contains(out, "Untagged: "+repo+"@"+digest) {
		t.Fatalf("removing the last tag left the digest reference:\n%s", out)
	}

	const box = "fws-e2e-prune"
	mustDocker(t, "run", "--name", box, "fws/toolbox:1", "put", "/x", "1")
	defer docker(t, "rm", "-f", box)
	first := strings.TrimSpace(mustDocker(t, "commit", box, "fws-e2e/prune:1"))
	second := strings.TrimSpace(mustDocker(t, "commit", "--change", "LABEL again=1", box, "fws-e2e/prune:1"))
	defer docker(t, "rmi", "fws-e2e/prune:1")
	if first == second {
		t.Fatal("the two commits made the same image")
	}
	if out := mustDocker(t, "images", "--filter", "dangling=true", "--quiet", "--no-trunc"); !strings.Contains(out, first) {
		t.Fatalf("the retagged commit is not dangling:\n%s", out)
	}
	if out := mustDocker(t, "image", "prune", "--force"); !strings.Contains(out, "deleted: "+first) && !strings.Contains(out, "Deleted: "+first) {
		t.Fatalf("image prune:\n%s", out)
	}
	if _, err := docker(t, "image", "inspect", first); err == nil {
		t.Fatal("the dangling image survived the prune")
	}
	if _, err := docker(t, "image", "inspect", second); err != nil {
		t.Fatalf("prune removed the tagged image: %v", err)
	}
}

// basicAuthRegistry serves a registry that admits only user:pass, through
// HTTP basic auth.
func basicAuthRegistry(t *testing.T, user, pass string) string {
	t.Helper()
	reg := registry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="e2e"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestRegistryLoginAndImport(t *testing.T) {
	// The CLI keeps logins in its config directory; this test's stay in its
	// own.
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	host := basicAuthRegistry(t, "fws", "s3cret")
	repo := host + "/e2e/private"
	mustDocker(t, "tag", "fws/toolbox:1", repo+":1")
	defer docker(t, "rmi", repo+":1")

	if _, err := docker(t, "push", repo+":1"); err == nil {
		t.Fatal("pushing to a registry that needs credentials succeeded without a login")
	}
	if _, err := dockerIn(t, "wrong\n", "login", host, "--username", "fws", "--password-stdin"); err == nil {
		t.Fatal("docker login accepted a wrong password")
	}
	if out, err := dockerIn(t, "s3cret\n", "login", host, "--username", "fws", "--password-stdin"); err != nil || !strings.Contains(out, "Login Succeeded") {
		t.Fatalf("docker login: %v\n%s", err, out)
	}
	if out := mustDocker(t, "push", repo+":1"); !strings.Contains(out, "Pushed") {
		t.Fatalf("push after login:\n%s", out)
	}
	if out := mustDocker(t, "push", repo+":1"); !strings.Contains(out, "Layer already exists") {
		t.Fatalf("pushing again:\n%s", out)
	}
	mustDocker(t, "rmi", repo+":1")
	mustDocker(t, "pull", repo+":1")
	mustDocker(t, "logout", host)
	mustDocker(t, "rmi", repo+":1")
	if _, err := docker(t, "pull", repo+":1"); err == nil {
		t.Fatal("pulling from a registry that needs credentials succeeded after logout")
	}
	mustDocker(t, "tag", "fws/toolbox:1", repo+":1")

	const box = "fws-e2e-export"
	mustDocker(t, "create", "--name", box, "fws/toolbox:1")
	defer docker(t, "rm", "-f", box)
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	pipe := exec.CommandContext(ctx, "sh", "-c", `docker export `+box+` | docker import --change 'ENTRYPOINT ["/toolbox"]' --message "from an export" - fws-e2e/imported:1`)
	if out, err := pipe.CombinedOutput(); err != nil || !strings.Contains(string(out), "sha256:") {
		t.Fatalf("docker export | docker import: %v\n%s", err, out)
	}
	defer docker(t, "rmi", "fws-e2e/imported:1")
	if got := strings.TrimSpace(mustDocker(t, "run", "--rm", "fws-e2e/imported:1", "echo", "imported")); got != "imported" {
		t.Fatalf("the imported image ran with output %q", got)
	}
	if h := mustDocker(t, "history", "fws-e2e/imported:1"); !strings.Contains(h, "from an export") {
		t.Fatalf("history of the imported image:\n%s", h)
	}
}

// dockerIn runs the Docker CLI with stdin.
func dockerIn(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// waitLog follows a container's log until a line holds text.
func waitLog(t *testing.T, name, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "logs", "--follow", name)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); cmd.Wait() }()
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if strings.Contains(sc.Text(), text) {
			return
		}
	}
	t.Fatalf("%s's log ended without %q", name, text)
}

func TestPublishUDPAndHostPortRanges(t *testing.T) {
	const name = "fws-e2e-udp"
	mustDocker(t, "run", "-d", "--name", name, "-p", "127.0.0.1::9999/udp", "fws/toolbox:1", "udpecho", "9999")
	defer docker(t, "rm", "-f", name)
	waitLog(t, name, "listening on udp")
	addr := strings.TrimSpace(mustDocker(t, "port", name, "9999/udp"))
	for _, client := range []string{"first", "second"} {
		c, err := net.Dial("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write([]byte(client)); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 100)
		n, err := c.Read(buf)
		if err != nil || string(buf[:n]) != "echo:"+client {
			t.Fatalf("the %s client got %q, %v through %s", client, buf[:n], err, addr)
		}
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lo := l.Addr().(*net.TCPAddr).Port
	l.Close()
	span := fmt.Sprintf("127.0.0.1:%d-%d:8080", lo, lo+1)
	for i := 0; i < 2; i++ {
		box := fmt.Sprintf("fws-e2e-range-%d", i)
		mustDocker(t, "run", "-d", "--name", box, "-p", span, "fws/toolbox:1", "udpecho", "8080")
		defer docker(t, "rm", "-f", box)
		if got := strings.TrimSpace(mustDocker(t, "port", box, "8080")); got != fmt.Sprintf("127.0.0.1:%d", lo+i) {
			t.Fatalf("container %d of the range published %s, want 127.0.0.1:%d", i, got, lo+i)
		}
	}
	if out, err := docker(t, "run", "-d", "--name", "fws-e2e-range-full", "-p", span, "fws/toolbox:1", "udpecho", "8080"); err == nil || !strings.Contains(out+err.Error(), "port is already allocated") {
		t.Fatalf("a third container on a two-port range: %v\n%s", err, out)
	}
	docker(t, "rm", "-f", "fws-e2e-range-full")
}

// classicBuild runs the classic builder through the Docker CLI.
func classicBuild(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append([]string{"build"}, args...)...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker build %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func imageID(t *testing.T, ref string) string {
	t.Helper()
	return strings.TrimSpace(mustDocker(t, "image", "inspect", "-f", "{{.ID}}", ref))
}

func TestClassicBuildCacheAndOnbuild(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", `FROM fws/toolbox:1
COPY greeting.txt /greeting.txt
RUN ["/toolbox", "put", "/ran.txt", "ran"]
CMD ["cat", "/greeting.txt"]
`)
	write("greeting.txt", "first\n")
	first := classicBuild(t, "-t", "e2e/cached:1", dir)
	defer docker(t, "rmi", "-f", "e2e/cached:1")
	if strings.Contains(first, "Using cache") {
		t.Fatalf("the first build used a cache:\n%s", first)
	}
	id := imageID(t, "e2e/cached:1")

	again := classicBuild(t, "-t", "e2e/cached:1", dir)
	if n := strings.Count(again, "Using cache"); n != 2 {
		t.Fatalf("an identical rebuild used the cache %d times, want 2 (COPY and RUN):\n%s", n, again)
	}
	if got := imageID(t, "e2e/cached:1"); got != id {
		t.Fatalf("an identical rebuild made %s, want the same image %s", got, id)
	}

	write("greeting.txt", "second\n")
	changed := classicBuild(t, "-t", "e2e/cached:1", dir)
	if strings.Contains(changed, "Using cache") {
		t.Fatalf("a changed COPY source left later steps cached:\n%s", changed)
	}
	if got := strings.TrimSpace(mustDocker(t, "run", "--rm", "e2e/cached:1")); got != "second" {
		t.Fatalf("the rebuilt image prints %q", got)
	}
	if out := classicBuild(t, "--no-cache", "-t", "e2e/cached:1", dir); strings.Contains(out, "Using cache") {
		t.Fatalf("--no-cache used the cache:\n%s", out)
	}

	mustDocker(t, "image", "prune", "--force")
	if out := classicBuild(t, "-t", "e2e/cached:1", dir); strings.Contains(out, "Using cache") {
		t.Fatalf("a build after docker image prune used the cache:\n%s", out)
	}

	base := t.TempDir()
	os.WriteFile(filepath.Join(base, "Dockerfile"), []byte("FROM fws/toolbox:1\nONBUILD COPY greeting.txt /onbuild.txt\n"), 0o644)
	classicBuild(t, "-t", "e2e/onbuild-base:1", base)
	defer docker(t, "rmi", "-f", "e2e/onbuild-base:1")
	if ob := mustDocker(t, "image", "inspect", "-f", "{{json .Config.OnBuild}}", "e2e/onbuild-base:1"); !strings.Contains(ob, "COPY greeting.txt /onbuild.txt") {
		t.Fatalf("the base image's ONBUILD triggers: %s", ob)
	}
	write("Dockerfile", "FROM e2e/onbuild-base:1\nCMD [\"cat\", \"/onbuild.txt\"]\n")
	out := classicBuild(t, "-t", "e2e/onbuild-child:1", dir)
	defer docker(t, "rmi", "-f", "e2e/onbuild-child:1")
	if !strings.Contains(out, "# Executing 1 build trigger") {
		t.Fatalf("the child build ran no trigger:\n%s", out)
	}
	if got := strings.TrimSpace(mustDocker(t, "run", "--rm", "e2e/onbuild-child:1")); got != "second" {
		t.Fatalf("the trigger's COPY left %q", got)
	}
	if ob := strings.TrimSpace(mustDocker(t, "image", "inspect", "-f", "{{json .Config.OnBuild}}", "e2e/onbuild-child:1")); ob != "null" && ob != "[]" {
		t.Fatalf("the child kept the triggers: %s", ob)
	}
}

// gitServer serves a one-commit repository holding files over smart HTTP,
// through git http-backend.
func gitServer(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@e2e", "GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@e2e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755)
		os.WriteFile(filepath.Join(work, name), []byte(body), 0o644)
	}
	git(work, "init", "--quiet", "-b", "main")
	git(work, "add", ".")
	git(work, "commit", "--quiet", "-m", "context")
	git(root, "clone", "--quiet", "--bare", work, filepath.Join(root, "srv", "ctx.git"))
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Join(root, "srv"), "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(srv.Close)
	return srv.URL + "/ctx.git"
}

// engineClient talks to the engine's socket directly, for API calls the
// Docker CLI makes no use of.
func engineClient() *http.Client {
	sock := strings.TrimPrefix(dockerHost, "unix://")
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
}

// The engine fetches a remote build context itself, as dockerd does for API
// clients that send one.
func TestRemoteBuildContext(t *testing.T) {
	repo := gitServer(t, map[string]string{
		"app/Dockerfile": "FROM fws/toolbox:1\nCOPY hello.txt /hello.txt\nCMD [\"cat\", \"/hello.txt\"]\n",
		"app/hello.txt":  "from git\n",
	})
	q := url.Values{"remote": {repo + "#main:app"}, "t": {"e2e/remote:1"}}
	res, err := engineClient().Post("http://engine/build?"+q.Encode(), "application/x-tar", nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(out), "Successfully tagged e2e/remote:1") {
		t.Fatalf("build from %s: %d\n%s", repo, res.StatusCode, out)
	}
	defer docker(t, "rmi", "-f", "e2e/remote:1")
	if got := strings.TrimSpace(mustDocker(t, "run", "--rm", "e2e/remote:1")); got != "from git" {
		t.Fatalf("the image built from Git prints %q", got)
	}
}

// Testcontainers' reaper removes what a test process left behind once the
// process goes: a helper starts a container and exits without removing it.
func TestTestcontainersReaper(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	events := exec.CommandContext(ctx, "docker", "events", "--filter", "type=container", "--filter", "event=destroy", "--format", "{{.Actor.ID}}")
	stream, err := events.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); events.Wait() }()

	helper := exec.CommandContext(ctx, "go", "run", "./testdata/reaped")
	out, err := helper.Output()
	if err != nil {
		t.Fatalf("the Testcontainers helper: %v\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))
	if len(id) != 64 {
		t.Fatalf("the helper printed %q, not a container ID", out)
	}
	// The helper's reaper carries its session's label; this test process's
	// own reaper, of another session, stays until this process ends.
	session := strings.TrimSpace(mustDocker(t, "inspect", "-f", `{{index .Config.Labels "org.testcontainers.sessionId"}}`, id))
	reapers := strings.Fields(mustDocker(t, "ps", "-aq", "--no-trunc", "--filter", "label=org.testcontainers.ryuk=true", "--filter", "label=org.testcontainers.sessionId="+session))
	if len(reapers) != 1 {
		t.Fatalf("session %s has reapers %v, want one", session, reapers)
	}
	want := map[string]bool{id: true}
	for _, r := range reapers {
		want[r] = true
	}
	sc := bufio.NewScanner(stream)
	for len(want) > 0 && sc.Scan() {
		delete(want, strings.TrimSpace(sc.Text()))
	}
	if len(want) > 0 {
		t.Fatalf("the reaper left %v in place", want)
	}
}

func TestDiffSizeDiskUsageAndSearch(t *testing.T) {
	const changed, fresh = "fws-e2e-diff", "fws-e2e-fresh"
	mustDocker(t, "run", "--name", changed, "fws/toolbox:1", "put", "/made.txt", "hi")
	defer docker(t, "rm", "-f", changed)
	mustDocker(t, "create", "--name", fresh, "fws/toolbox:1")
	defer docker(t, "rm", "-f", fresh)
	if out := mustDocker(t, "diff", changed); !strings.Contains(out, "A /made.txt") {
		t.Fatalf("docker diff of a container that wrote /made.txt:\n%s", out)
	}
	if out := strings.TrimSpace(mustDocker(t, "diff", fresh)); out != "" {
		t.Fatalf("docker diff of a fresh container lists changes:\n%s", out)
	}
	if size := mustDocker(t, "ps", "-a", "--size", "--filter", "name="+changed, "--format", "{{.Size}}"); !strings.Contains(size, "B (virtual") || strings.HasPrefix(size, "0B") {
		t.Fatalf("docker ps --size shows %q", size)
	}
	if out := mustDocker(t, "system", "df"); !strings.Contains(out, "Images") || !strings.Contains(out, "Build Cache") {
		t.Fatalf("docker system df:\n%s", out)
	}
	if out := mustDocker(t, "system", "df", "--verbose"); !strings.Contains(out, "fws/toolbox") {
		t.Fatalf("docker system df --verbose:\n%s", out)
	}
	if out := mustDocker(t, "search", "--limit", "5", "--filter", "is-official=true", "postgres"); !strings.Contains(out, "postgres") {
		t.Fatalf("docker search:\n%s", out)
	}
}

// A process that leaves the container's process group is still the
// container's: docker top lists it and docker stop ends it.
func TestProcessesOutsideTheGroupStayContained(t *testing.T) {
	const name = "fws-e2e-detach"
	mustDocker(t, "run", "-d", "--name", name, "fws/toolbox:1", "detach")
	defer docker(t, "rm", "-f", name)
	waitLog(t, name, "detached")
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(mustDocker(t, "logs", name)), "detached %d", &pid); err != nil || pid == 0 {
		t.Fatalf("the container's log names no detached process: %v", err)
	}
	if top := mustDocker(t, "top", name, "-o", "pid"); !strings.Contains(top, strconv.Itoa(pid)) {
		t.Fatalf("docker top does not list detached process %d:\n%s", pid, top)
	}
	mustDocker(t, "stop", "--time", "1", name)
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("detached process %d outlived docker stop: kill(pid, 0) = %v", pid, err)
	}
}
