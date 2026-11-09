//go:build e2e && darwin

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// project is a Compose project in a directory of its own.
type project struct {
	t    *testing.T
	name string
	dir  string
	env  []string
}

func newProject(t *testing.T, name string, files map[string]string, env ...string) *project {
	t.Helper()
	dir := t.TempDir()
	for f, content := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := &project{t: t, name: name, dir: dir, env: env}
	t.Cleanup(func() { p.run("down", "-t", "0", "--volumes", "--remove-orphans") })
	return p
}

func (p *project) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := stacksOnTimeout(exec.CommandContext(ctx, "docker-compose", append([]string{"-p", p.name, "--project-directory", p.dir}, args...)...))
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), p.env...)
	return cmd
}

func (p *project) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := p.command(ctx, args...).CombinedOutput()
	return string(out), err
}

func (p *project) must(args ...string) string {
	p.t.Helper()
	out, err := p.run(args...)
	if err != nil {
		p.t.Fatalf("docker-compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// py runs a Python program in a service and returns what it printed.
func (p *project) py(service, program string) string {
	p.t.Helper()
	return p.must("exec", "-T", service, "python3", "-c", program)
}

const sleeper = `["python3", "-c", "import time; time.sleep(600)"]`

// include, extends and profiles assemble the services of one project from
// several files, as Compose does before it asks the engine for anything.
func TestComposeIncludeExtendsAndProfiles(t *testing.T) {
	p := newProject(t, "e2e-assemble", map[string]string{
		"common.yaml": `services:
  base:
    image: fws/python:3.14
    command: ` + sleeper + `
    environment: {FROM_BASE: "base"}
`,
		"included.yaml": `services:
  helper:
    image: fws/python:3.14
    command: ` + sleeper + `
`,
		"compose.yaml": `include: [included.yaml]
services:
  app:
    extends: {file: common.yaml, service: base}
    environment: {OWN: "own"}
  debug:
    image: fws/python:3.14
    command: ` + sleeper + `
    profiles: [debug]
`,
	})
	p.must("up", "-d", "--wait")
	if got := p.must("ps", "--services", "--status", "running"); got != "app\nhelper\n" {
		t.Errorf("services without the profile: %q", got)
	}
	if got := p.py("app", "import os; print(os.environ['FROM_BASE'], os.environ['OWN'])"); got != "base own\n" {
		t.Errorf("the extended service's environment: %q", got)
	}
	p.must("--profile", "debug", "up", "-d", "--wait")
	if got := p.must("--profile", "debug", "ps", "--services", "--status", "running"); got != "app\ndebug\nhelper\n" {
		t.Errorf("services with the profile: %q", got)
	}
}

// Configs and secrets reach a service from files, which Compose mounts,
// and from inline content and the environment, which it copies into the
// container before it starts, with the modes they ask for.
func TestComposeConfigsAndSecrets(t *testing.T) {
	p := newProject(t, "e2e-configs", map[string]string{
		"app.conf":  "from a file\n",
		"token.txt": "file-token\n",
		"compose.yaml": `configs:
  from_file: {file: ./app.conf}
  inline: {content: "inline ${GREETING}"}
  from_env: {environment: APP_SETTING}
secrets:
  token: {file: ./token.txt}
  api_key: {environment: API_KEY}
services:
  app:
    image: fws/python:3.14
    command: ` + sleeper + `
    configs:
      - source: from_file
        target: /etc/app.conf
      - inline
      - source: from_env
        target: /etc/setting
        mode: 0440
    secrets:
      - token
      - source: api_key
        target: api.key
`,
	}, "GREETING=hello", "APP_SETTING=fast", "API_KEY=k123")
	p.must("up", "-d", "--wait")
	const show = `import os
for f in ["/etc/app.conf", "/inline", "/etc/setting", "/run/secrets/token", "/run/secrets/api.key"]:
    print(f, repr(open(f).read()), oct(os.stat(f).st_mode & 0o777))`
	want := "/etc/app.conf 'from a file\\n' 0o644\n" +
		"/inline 'inline hello' 0o444\n" +
		"/etc/setting 'fast' 0o440\n" +
		"/run/secrets/token 'file-token\\n' 0o644\n" +
		"/run/secrets/api.key 'k123' 0o444\n"
	if got := p.py("app", show); got != want {
		t.Errorf("configs and secrets:\n%s\nwant:\n%s", got, want)
	}
}

// compose watch syncs a changed file into the running service and
// restarts it for one the service reads as it starts, as its develop
// section says.
func TestComposeWatch(t *testing.T) {
	p := newProject(t, "e2e-watch", map[string]string{
		"src/note.txt": "one",
		"conf/mode":    "first",
		"Dockerfile":   "FROM fws/python:3.14\nCOPY src /app\nCOPY conf /conf\n",
		"compose.yaml": `services:
  app:
    build: .
    command: ["python3", "-c", "import time; open('/tmp/mode', 'w').write(open('/conf/mode').read()); time.sleep(600)"]
    develop:
      watch:
        - action: sync
          path: ./src
          target: /app
        - action: sync+restart
          path: ./conf
          target: /conf
`,
	})
	p.must("up", "-d", "--build", "--wait")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	watch := p.command(ctx, "watch", "--no-up")
	out, err := watch.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	watch.Stderr = watch.Stdout
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		watch.Process.Signal(os.Interrupt)
		watch.Wait()
	}()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	var seen []string
	await := func(what string) {
		t.Helper()
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("watch ended before %q:\n%s", what, strings.Join(seen, "\n"))
				}
				seen = append(seen, l)
				if strings.Contains(l, what) {
					return
				}
			case <-ctx.Done():
				t.Fatalf("watch printed no %q:\n%s", what, strings.Join(seen, "\n"))
			}
		}
	}
	await("Watch enabled")
	if err := os.WriteFile(filepath.Join(p.dir, "src", "note.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	await("Syncing")
	if got := p.py("app", "print(open('/app/note.txt').read())"); got != "two\n" {
		t.Errorf("the synced file: %q", got)
	}
	if err := os.WriteFile(filepath.Join(p.dir, "conf", "mode"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	await("restarted")
	if got := p.py("app", "print(open('/tmp/mode').read())"); got != "second\n" {
		t.Errorf("the restarted service read %q", got)
	}
}

// A service runs under the engine's init, in its own UTS namespace with a
// domain name, with supplementary groups, a /dev/shm of the size it asks
// for, and its post_start hook run.
func TestComposeServiceSettings(t *testing.T) {
	p := newProject(t, "e2e-settings", map[string]string{
		"compose.yaml": `services:
  app:
    image: fws/python:3.14
    init: true
    hostname: web
    domainname: example.com
    group_add: [nobody, "777"]
    shm_size: 4mb
    command: ["python3", "-c", "import os, time; open('/tmp/ppid', 'w').write(str(os.getppid())); time.sleep(600)"]
    post_start:
      - command: ["python3", "-c", "open('/tmp/post_start', 'w').write('ran')"]
`,
	})
	p.must("up", "-d", "--wait")
	const probe = `import ctypes, os, socket, errno
from multiprocessing import shared_memory, resource_tracker
print("ppid", open("/tmp/ppid").read())
print("groups", sorted(os.getgroups()))
buf = ctypes.create_string_buffer(256)
ctypes.CDLL(None).getdomainname(buf, 256)
print("uts", socket.gethostname(), buf.value.decode())
print("hosts", [l for l in open("/etc/hosts").read().splitlines() if "web" in l][0].split("\t")[1])
first = shared_memory.SharedMemory(create=True, size=3 << 20)
try:
    shared_memory.SharedMemory(create=True, size=2 << 20)
    print("shm", "fits")
except OSError as e:
    print("shm", errno.errorcode[e.errno])
first.close()
first.unlink()
print("post_start", open("/tmp/post_start").read())`
	want := "ppid 1\ngroups [777, 65534]\nuts web example.com\nhosts web.example.com web\nshm ENOSPC\npost_start ran\n"
	if got := p.py("app", probe); got != want {
		t.Errorf("the service saw:\n%s\nwant:\n%s", got, want)
	}
	var inspect []struct {
		HostConfig struct {
			Init    *bool
			ShmSize int64
		}
	}
	id := strings.TrimSpace(p.must("ps", "-q", "app"))
	if err := json.Unmarshal([]byte(mustDocker(t, "inspect", id)), &inspect); err != nil || len(inspect) != 1 ||
		inspect[0].HostConfig.Init == nil || !*inspect[0].HostConfig.Init || inspect[0].HostConfig.ShmSize != 4<<20 {
		t.Errorf("inspect: %+v, %v", inspect, err)
	}
	// Docker's default /dev/shm is 64 MiB.
	out := mustDocker(t, "run", "--rm", "fws/python:3.14", "python3", "-c",
		"from multiprocessing import shared_memory\ns = shared_memory.SharedMemory(create=True, size=60 << 20); s.close(); s.unlink()\n"+
			"try:\n    shared_memory.SharedMemory(create=True, size=65 << 20)\nexcept OSError as e:\n    print(e.errno)")
	if out != "28\n" {
		t.Errorf("the default shared memory size: %q", out)
	}
}

// Settings the engine cannot honour fail the container's creation with
// the reason, rather than pass unheeded.
func TestSettingsTheEngineRefuses(t *testing.T) {
	for _, c := range []struct {
		flags []string
		want  string
	}{
		{[]string{"--dns", "1.1.1.1"}, "DNS servers, search domains and options are not supported"},
		{[]string{"--dns-search", "example.com"}, "DNS servers, search domains and options are not supported"},
		{[]string{"--oom-score-adj", "500"}, "OOM score adjustments are not supported"},
		{[]string{"--storage-opt", "size=1G"}, "storage options are not supported"},
		{[]string{"--isolation", "hyperv"}, "invalid isolation"},
	} {
		args := append(append([]string{"create"}, c.flags...), "fws/python:3.14", "true")
		if out, err := docker(t, args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("docker %s: %q, %v; want an error with %q", strings.Join(args, " "), out, err, c.want)
			if err == nil {
				docker(t, "rm", strings.TrimSpace(out))
			}
		}
	}
}
