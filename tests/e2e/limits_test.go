//go:build e2e && darwin

package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Past its memory limit a container's largest process is killed, as the
// kernel's OOM killer kills in a cgroup, and the container reports it.
func TestMemoryLimit(t *testing.T) {
	const name = "fws-e2e-oom"
	defer docker(t, "rm", "-f", name)
	_, err := docker(t, "run", "--name", name, "--memory", "32m", "fws/toolbox:1", "alloc", "512")
	if err == nil || !strings.Contains(err.Error(), "exit status 137") {
		t.Fatalf("a container past its memory limit ran on: %v", err)
	}
	if s := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.ExitCode}} {{.State.OOMKilled}}", name)); s != "137 true" {
		t.Fatalf("the container's state reads %q, want an OOM kill", s)
	}
	// Under the limit the same program runs.
	if out := mustDocker(t, "run", "--rm", "-d", "--memory", "256m", "fws/toolbox:1", "alloc", "16"); out == "" {
		t.Fatal("no container ID")
	}
}

// A CPU quota holds a busy container to its share of a CPU.
func TestCPUQuota(t *testing.T) {
	const name = "fws-e2e-cpus"
	mustDocker(t, "run", "-d", "--name", name, "--cpus", "0.5", "fws/toolbox:1", "burn", "4s")
	defer docker(t, "rm", "-f", name)
	waitLog(t, name, "cpu ")
	var ms int
	if _, err := fmt.Sscanf(strings.TrimSpace(mustDocker(t, "logs", name)), "cpu %d", &ms); err != nil {
		t.Fatal(err)
	}
	// Half a CPU for four seconds is two seconds of CPU time; unthrottled
	// the loop takes four.
	if ms < 1200 || ms > 2800 {
		t.Fatalf("a container at half a CPU used %dms of CPU in 4s", ms)
	}
	out := mustDocker(t, "stats", "--no-stream", "--format", "{{.CPUPerc}}", name)
	if !strings.HasSuffix(strings.TrimSpace(out), "%") {
		t.Fatalf("docker stats: %q", out)
	}
	if s := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.HostConfig.NanoCpus}}", name)); s != "500000000" {
		t.Fatalf("inspect reports %s CPUs", s)
	}
}

// Forks past the pids limit end at once.
func TestPidsLimit(t *testing.T) {
	const name = "fws-e2e-pids"
	mustDocker(t, "run", "-d", "--name", name, "--pids-limit", "6", "fws/toolbox:1", "forkmany", "20")
	defer docker(t, "rm", "-f", name)
	waitLog(t, name, "started 20")
	deadline := time.Now().Add(30 * time.Second)
	for {
		n, err := strconv.Atoi(strings.TrimSpace(mustDocker(t, "stats", "--no-stream", "--format", "{{.PIDs}}", name)))
		if err != nil {
			t.Fatal(err)
		}
		if n == 6 {
			break
		}
		if n < 6 || time.Now().After(deadline) {
			t.Fatalf("the container runs %d processes, want its limit of 6", n)
		}
	}
}

func TestUlimits(t *testing.T) {
	if _, err := docker(t, "run", "--rm", "--ulimit", "nofile=64:64", "fws/toolbox:1", "openfiles", "100"); err == nil || !strings.Contains(err.Error(), "too many open files") {
		t.Fatalf("100 files under a limit of 64: %v", err)
	}
	if out := mustDocker(t, "run", "--rm", "--ulimit", "nofile=256:256", "fws/toolbox:1", "openfiles", "100"); !strings.Contains(out, "opened 100") {
		t.Fatalf("100 files under a limit of 256: %q", out)
	}
	if _, err := docker(t, "run", "--rm", "--ulimit", "nproc=10", "fws/toolbox:1", "echo"); err == nil || !strings.Contains(err.Error(), "nproc is not supported") {
		t.Fatalf("ulimit nproc: %v", err)
	}
	if _, err := docker(t, "run", "--rm", "--cpuset-cpus", "0", "fws/toolbox:1", "echo"); err == nil || !strings.Contains(err.Error(), "cpusets are not supported") {
		t.Fatalf("cpuset: %v", err)
	}
}

func TestReadOnlyRoot(t *testing.T) {
	if _, err := docker(t, "run", "--rm", "--read-only", "fws/toolbox:1", "put", "/written", "x"); err == nil {
		t.Fatal("a container wrote to its read-only root")
	}
	mustDocker(t, "run", "--rm", "--read-only", "--tmpfs", "/scratch", "fws/toolbox:1", "put", "/scratch/f", "x")
	mustDocker(t, "run", "--rm", "fws/toolbox:1", "put", "/written", "x")
}

func TestCapabilitiesAndSecurityOptions(t *testing.T) {
	mustDocker(t, "run", "--rm", "--cap-drop", "ALL", "--cap-add", "CHOWN", "--security-opt", "no-new-privileges", "fws/toolbox:1", "echo")
	if _, err := docker(t, "run", "--rm", "--cap-add", "SYS_ADMIN", "fws/toolbox:1", "echo"); err == nil || !strings.Contains(err.Error(), "needs root") {
		t.Fatalf("cap-add SYS_ADMIN: %v", err)
	}
	if _, err := docker(t, "run", "--rm", "--privileged", "fws/toolbox:1", "echo"); err == nil {
		t.Fatal("a privileged container ran")
	}
	if _, err := docker(t, "run", "--rm", "--security-opt", "seccomp=/dev/null", "fws/toolbox:1", "echo"); err == nil {
		t.Fatal("a seccomp profile was accepted")
	}
}

// Private IPC confines POSIX shared memory to the container's names;
// container:<name> shares another's; none denies IPC.
func TestIPCModes(t *testing.T) {
	mustDocker(t, "run", "--rm", "fws/toolbox:1", "shm", "+own")
	if _, err := docker(t, "run", "--rm", "fws/toolbox:1", "shm", "fwsnotmine.x"); err == nil {
		t.Fatal("a container opened shared memory outside its prefix")
	}
	mustDocker(t, "run", "--rm", "--ipc", "host", "fws/toolbox:1", "shm", "fwshost.e2e")
	if _, err := docker(t, "run", "--rm", "--ipc", "none", "fws/toolbox:1", "shm", "+own"); err == nil {
		t.Fatal("a container without IPC opened shared memory")
	}
	const owner = "fws-e2e-ipc-owner"
	mustDocker(t, "run", "-d", "--name", owner, "--ipc", "shareable", "fws/toolbox:1", "hold")
	defer docker(t, "rm", "-f", owner)
	prefix := ""
	for _, kv := range strings.Fields(mustDocker(t, "exec", owner, "/toolbox", "env")) {
		if v, ok := strings.CutPrefix(kv, "SANDBOX_IPC_PREFIX="); ok {
			prefix = v
		}
	}
	if prefix == "" {
		t.Fatal("the container's environment names no IPC prefix")
	}
	mustDocker(t, "run", "--rm", "--ipc", "container:"+owner, "fws/toolbox:1", "shm", prefix+"shared")
}

// liveDaemon is an engine of the test's own.
type liveDaemon struct {
	t    *testing.T
	root string
	sock string
	host string
	cmd  *exec.Cmd
}

func startLiveDaemon(t *testing.T, root, sock string) *liveDaemon {
	return startDaemon(t, root, sock, "-live-restore")
}

func startDaemon(t *testing.T, root, sock string, args ...string) *liveDaemon {
	t.Helper()
	d := &liveDaemon{t: t, root: root, sock: sock, host: "unix://" + sock}
	d.cmd = exec.Command(fwsdBin, append([]string{"-root", root, "-socket", sock,
		"-port-base", "31000", "-port-limit", "31500", "-block-size", "50", "-buildkit=false"}, args...)...)
	stderr, err := d.cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stderr)
		announced := false
		for sc.Scan() {
			line := sc.Text()
			t.Log("live fwsd:", line)
			if crashLine(line) {
				t.Errorf("the engine crashed: %s", line)
			}
			if !announced && strings.Contains(line, "serving the Docker API") {
				announced = true
				close(ready)
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("the live-restore engine did not start")
	}
	return d
}

func (d *liveDaemon) stop() {
	d.cmd.Process.Signal(syscall.SIGTERM)
	if err := d.cmd.Wait(); err != nil {
		d.t.Fatalf("the engine exited with %v", err)
	}
}

func (d *liveDaemon) docker(args ...string) string {
	d.t.Helper()
	out, err := dockerAt(d.t, d.host, args...)
	if err != nil {
		d.t.Fatal(err)
	}
	return out
}

// With live restore a container runs on while the engine stops, keeps its
// published port, and the next engine takes it back.
func TestLiveRestore(t *testing.T) {
	home, _ := os.UserHomeDir()
	root, err := os.MkdirTemp(home, ".lr")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	sock := filepath.Join(home, ".lr.sock")
	d := startLiveDaemon(t, root, sock)
	archive, _ := filepath.Glob(filepath.Join(imagesDir, "*toolbox*.tar"))
	if len(archive) != 1 {
		t.Fatalf("toolbox archive: %v", archive)
	}
	d.docker("load", "-i", archive[0])
	const name = "lr"
	d.docker("run", "-d", "--name", name, "-p", "127.0.0.1::8080", "--memory", "128m", "fws/toolbox:1")
	defer func() {
		dockerAt(t, d.host, "rm", "-f", name)
		d.stop()
	}()
	hostPort := strings.TrimSpace(d.docker("port", name, "8080"))
	pid, _ := strconv.Atoi(strings.TrimSpace(d.docker("inspect", "-f", "{{.State.Pid}}", name)))
	get := func() error {
		c := http.Client{Timeout: 5 * time.Second}
		res, err := c.Get("http://" + hostPort + "/healthz")
		if err != nil {
			return err
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("healthz: %s", res.Status)
		}
		return nil
	}
	waitLiveLog(t, d.host, name, "toolbox listening")
	if err := get(); err != nil {
		t.Fatal(err)
	}
	waitLiveHealthy(t, d.host, name)
	t.Logf("the container's supervisor holds %d KiB resident", supervisorRSS(t, root))

	d.stop()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the container's process %d ended with the engine: %v", pid, err)
	}
	if err := get(); err != nil {
		t.Fatalf("the published port stopped serving while no engine ran: %v", err)
	}

	d = startLiveDaemon(t, root, sock)
	if s := strings.TrimSpace(d.docker("inspect", "-f", "{{.State.Status}} {{.State.Pid}}", name)); s != "running "+strconv.Itoa(pid) {
		t.Fatalf("after the engine restarted the container reads %q, want running as %d", s, pid)
	}
	if err := get(); err != nil {
		t.Fatal(err)
	}
	if s := strings.TrimSpace(d.docker("inspect", "-f", "{{.State.Health.Status}}", name)); s != "healthy" {
		t.Fatalf("after the engine reconnected the container's health reads %q, want healthy", s)
	}
	d.docker("update", "--cpus", "1", name)
	d.docker("stop", "--time", "2", name)
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the container's process %d outlived docker stop: %v", pid, err)
	}
	if s := strings.TrimSpace(d.docker("inspect", "-f", "{{.State.Status}}", name)); s != "exited" {
		t.Fatalf("after docker stop the container reads %q", s)
	}
	if err := get(); err == nil {
		t.Fatal("the published port still serves after the container stopped")
	}
}

// Without live restore an engine that starts ends the containers an
// earlier one left running, here one that died without stopping them.
func TestEngineRestartEndsContainersWithoutLiveRestore(t *testing.T) {
	home, _ := os.UserHomeDir()
	root, err := os.MkdirTemp(home, ".nl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	sock := filepath.Join(home, ".nl.sock")
	d := startDaemon(t, root, sock)
	archive, _ := filepath.Glob(filepath.Join(imagesDir, "*toolbox*.tar"))
	if len(archive) != 1 {
		t.Fatalf("toolbox archive: %v", archive)
	}
	d.docker("load", "-i", archive[0])
	if s := strings.TrimSpace(d.docker("info", "-f", "{{.LiveRestoreEnabled}}")); s != "false" {
		t.Fatalf("docker info reports live restore %s", s)
	}
	const name = "nl"
	d.docker("run", "-d", "--name", name, "fws/toolbox:1", "hold")
	pid, _ := strconv.Atoi(strings.TrimSpace(d.docker("inspect", "-f", "{{.State.Pid}}", name)))
	d.cmd.Process.Kill()
	d.cmd.Wait()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the container's process %d did not outlive the engine's crash: %v", pid, err)
	}
	d = startDaemon(t, root, sock)
	defer d.stop()
	if s := strings.TrimSpace(d.docker("inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", name)); s != "exited 137" {
		t.Fatalf("after the engine restarted the container reads %q, want exited by SIGKILL", s)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the container's process %d outlived the engine's restart: %v", pid, err)
	}
	d.docker("rm", name)
}

func waitLiveLog(t *testing.T, host, name, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "logs", "--follow", name)
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+host)
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

func waitLiveHealthy(t *testing.T, host, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "events", "--since", "0",
		"--filter", "container="+name, "--filter", "event=health_status", "--format", "{{.Action}}")
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+host)
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
		if strings.TrimSpace(sc.Text()) == "health_status: healthy" {
			return
		}
	}
	t.Fatalf("%s never reported healthy", name)
}

// supervisorRSS is the resident memory of the shim of the one container
// under the engine root, in KiB, as ps reports it.
func supervisorRSS(t *testing.T, root string) int {
	t.Helper()
	out, err := exec.Command("/bin/ps", "-ax", "-o", "rss=,command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[2] == "shim" && strings.HasPrefix(f[3], root) {
			kib, _ := strconv.Atoi(f[0])
			return kib
		}
	}
	t.Fatalf("no supervisor runs under %s:\n%s", root, out)
	return 0
}
