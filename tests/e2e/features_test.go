//go:build e2e && darwin

package e2e

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	// Ryuk, Testcontainers' reaper, ships only as a Linux image; the test
	// terminates its own container instead.
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	redis, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "fws/redis:7",
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
	mustDocker(t, "run", "-d", "--name", "toptest", "fws/toolbox:1")
	defer docker(t, "rm", "-f", "toptest")

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
}

// waitHealthy blocks until the container reports healthy, reading Docker's
// event stream from since so an event emitted before the call is not missed.
func waitHealthy(t *testing.T, name string, since time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "events", "--since", strconv.FormatInt(since.Unix(), 10),
		"--filter", "container="+name, "--filter", "event=health_status", "--format", "{{.Action}}")
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
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "health_status: healthy" {
			return
		}
	}
	t.Fatalf("%s never reported healthy: %v", name, ctx.Err())
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
	if _, err := docker(t, "update", "--memory", "64m", "pausetest"); err == nil || !strings.Contains(err.Error(), "memory limits are not supported") {
		t.Fatalf("memory update: %v", err)
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

	dir := t.TempDir()
	exported := filepath.Join(dir, "export.tar")
	mustDocker(t, "export", "-o", exported, box)
	if !tarHas(t, exported, "greeting") {
		t.Fatal("the export does not hold /greeting")
	}

	saved := filepath.Join(dir, "save.tar")
	mustDocker(t, "save", "-o", saved, "fws-e2e/committed:1")
	id := strings.TrimSpace(mustDocker(t, "image", "inspect", "-f", "{{.Id}}", "fws-e2e/committed:1"))
	mustDocker(t, "rmi", "fws-e2e/committed:1")
	mustDocker(t, "load", "-i", saved)
	if again := strings.TrimSpace(mustDocker(t, "image", "inspect", "-f", "{{.Id}}", "fws-e2e/committed:1")); again != id {
		t.Fatalf("save and load changed the image ID from %s to %s", id, again)
	}

	reg := httptest.NewServer(registry.New())
	defer reg.Close()
	pushed := strings.TrimPrefix(reg.URL, "http://") + "/e2e/committed:pushed"
	mustDocker(t, "tag", "fws-e2e/committed:1", pushed)
	if out := mustDocker(t, "push", pushed); !strings.Contains(out, "digest: sha256:") {
		t.Fatalf("push output:\n%s", out)
	}
	mustDocker(t, "rmi", pushed)
	mustDocker(t, "pull", pushed)
	defer docker(t, "rmi", pushed)
	if out := mustDocker(t, "run", "--rm", pushed); strings.TrimSpace(out) != "committed" {
		t.Fatalf("the pushed and pulled image ran with output %q", out)
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
