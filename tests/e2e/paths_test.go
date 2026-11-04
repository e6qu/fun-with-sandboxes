//go:build e2e && darwin

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Absolute paths in a command's arguments name the container's files, as
// in Docker: the container library maps them into the root.
func TestArgumentsNameTheContainersFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.mjs"), []byte("console.log('node', process.cwd())\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "hello.py"), []byte("import os\nprint('python', os.getcwd())\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "hello.ts"), []byte("console.log('bun', process.cwd());\n"), 0o644)
	for _, c := range []struct {
		image string
		args  []string
		want  string
	}{
		{"fws/node:26", []string{"node", "/app/hello.mjs"}, "node /app"},
		{"fws/python:3.14", []string{"python3", "/app/hello.py"}, "python /app"},
		{"fws/bun:1", []string{"bun", "run", "/app/hello.ts"}, "bun /app"},
		// The image's own shell runs a command line with the container's
		// paths, its children too.
		{"fws/python:3.14", []string{"/bin/sh", "-c", "cd /app && python3 hello.py"}, "python /app"},
	} {
		out, err := docker(t, append([]string{"run", "--rm", "-v", dir + ":/app", "-w", "/app", c.image}, c.args...)...)
		if err != nil || strings.TrimSpace(out) != c.want {
			t.Errorf("%s %q: %q, %v; want %q", c.image, c.args, out, err, c.want)
		}
	}
}

func TestEntrypointAndCmdWithAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(`FROM fws/toolbox:1
RUN ["/toolbox", "put", "/etc/greeting", "from the image"]
ENTRYPOINT ["/toolbox", "cat"]
CMD ["/etc/greeting"]
`), 0o644)
	if out, err := exec.Command("docker", "buildx", "build", "--load", "-t", "e2e/abs:1", dir).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	defer docker(t, "rmi", "e2e/abs:1")
	if out := mustDocker(t, "run", "--rm", "e2e/abs:1"); out != "from the image\n" {
		t.Fatalf("CMD: %q", out)
	}
	mustDocker(t, "run", "--rm", "--entrypoint", "/toolbox", "e2e/abs:1", "put", "/tmp/elsewhere", "x")
	if _, err := docker(t, "run", "--rm", "e2e/abs:1", "/etc/missing"); err == nil {
		t.Fatal("cat of a file the container lacks succeeded")
	}
}

// The container's user, hostname and working directory are what Docker's
// options name, though the host runs the processes as the engine's user.
func TestUserHostnameAndWorkdir(t *testing.T) {
	whoami := func(args ...string) string {
		return strings.TrimSpace(mustDocker(t, append(append([]string{"run", "--rm"}, args...), "fws/toolbox:1", "whoami")...))
	}
	if got := whoami("--hostname", "web-1", "-w", "/tmp"); got != "uid=0 gid=0 user=root host=web-1 cwd=/tmp" {
		t.Errorf("default user: %q", got)
	}
	if got := whoami("--user", "1000:50"); !strings.HasPrefix(got, "uid=1000 gid=50 ") {
		t.Errorf("--user 1000:50: %q", got)
	}
	if _, err := docker(t, "run", "--rm", "--user", "nobody", "fws/toolbox:1", "whoami"); err == nil || !strings.Contains(err.Error(), "unable to find user nobody") {
		t.Errorf("an unknown user: %v", err)
	}
	const name = "fws-e2e-identity"
	mustDocker(t, "run", "-d", "--name", name, "--hostname", "box", "fws/toolbox:1", "hold")
	defer docker(t, "rm", "-f", name)
	if got := strings.TrimSpace(mustDocker(t, "exec", "-u", "42", name, "/toolbox", "whoami")); !strings.HasPrefix(got, "uid=42 gid=0 ") || !strings.Contains(got, "host=box") {
		t.Errorf("exec -u 42: %q", got)
	}
}

// A build step runs as the Dockerfile's USER.
func TestBuildUser(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(`FROM fws/toolbox:1
USER 1000:1000
RUN ["/toolbox", "whoami"]
`), 0o644)
	out, err := exec.Command("docker", "buildx", "build", "--no-cache", "--progress", "plain", dir).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "uid=1000 gid=1000") {
		t.Fatalf("build as USER 1000: %v\n%s", err, out)
	}
}

// On the host's UTS namespace the container reads the host's hostname, and
// may not name one of its own, as in Docker.
func TestHostUTSNamespace(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if got := mustDocker(t, "run", "--rm", "--uts", "host", "fws/toolbox:1", "whoami"); !strings.Contains(got, " host="+host+" ") {
		t.Errorf("--uts host: %q, want the host's hostname %q", got, host)
	}
	if _, err := docker(t, "run", "--rm", "--uts", "host", "--hostname", "box", "fws/toolbox:1", "whoami"); err == nil || !strings.Contains(err.Error(), "conflicting options: hostname and the UTS mode") {
		t.Errorf("--uts host with --hostname: %v", err)
	}
}

// --volumes-from mounts another container's volumes and binds where it
// mounts them: read-write as it has them, read-only with :ro, and under a
// mount of the container's own at the same place.
func TestVolumesFrom(t *testing.T) {
	const src, vol = "fws-e2e-vfsrc", "fws-e2e-vf"
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "from-host"), []byte("bound\n"), 0o644)
	mustDocker(t, "create", "--name", src, "-v", vol+":/data", "-v", dir+":/bind", "fws/toolbox:1")
	defer func() {
		docker(t, "rm", "-f", src)
		docker(t, "volume", "rm", "-f", vol)
	}()
	run := func(args ...string) (string, error) {
		return docker(t, append([]string{"run", "--rm"}, args...)...)
	}
	if _, err := run("--volumes-from", src, "fws/toolbox:1", "put", "/data/shared", "through volumes-from"); err != nil {
		t.Fatal(err)
	}
	if got, err := run("-v", vol+":/v", "fws/toolbox:1", "cat", "/v/shared"); err != nil || got != "through volumes-from\n" {
		t.Errorf("the source's volume holds %q (%v), want what the other container wrote", got, err)
	}
	if got, err := run("--volumes-from", src, "fws/toolbox:1", "cat", "/bind/from-host"); err != nil || got != "bound\n" {
		t.Errorf("the source's bind reads %q (%v)", got, err)
	}
	if _, err := run("--volumes-from", src+":ro", "fws/toolbox:1", "put", "/data/denied", "x"); err == nil {
		t.Error("a container wrote to a volume it mounts read-only from another")
	}
	mine := t.TempDir()
	os.WriteFile(filepath.Join(mine, "from-host"), []byte("mine\n"), 0o644)
	if got, err := run("--volumes-from", src, "-v", mine+":/bind", "fws/toolbox:1", "cat", "/bind/from-host"); err != nil || got != "mine\n" {
		t.Errorf("under the container's own bind: %q (%v), want its own", got, err)
	}
	// The CLI takes any 404 from create for a missing image and pulls it,
	// unless told not to.
	if _, err := run("--pull", "never", "--volumes-from", "fws-e2e-no-such", "fws/toolbox:1", "echo"); err == nil || !strings.Contains(err.Error(), "No such container") {
		t.Errorf("volumes from a missing container: %v", err)
	}
}
