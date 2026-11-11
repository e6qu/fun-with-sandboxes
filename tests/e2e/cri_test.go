//go:build e2e && darwin

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// crictl runs cri-tools' CRI client against the engine, which serves the
// Container Runtime Interface on its socket beside Docker's API, and
// returns what it printed on stdout; its warnings go to stderr, which a
// failure's error carries.
func crictl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "crictl", append([]string{
		"--runtime-endpoint", dockerHost, "--image-endpoint", dockerHost, "--timeout", "2m",
	}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		err = fmt.Errorf("%w\n%s", err, stderr.String())
	}
	return string(out), err
}

func mustCrictl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := crictl(t, args...)
	if err != nil {
		t.Fatalf("crictl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func writeJSON(t *testing.T, path string, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The engine runs a pod sandbox and its containers for a CRI client: the
// containers share the sandbox's network and its published port, the
// runtime writes their output where the client asks, in CRI's format, and
// runs commands in them, and the sandbox goes with its containers.
func TestCRIPodSandbox(t *testing.T) {
	if out := mustCrictl(t, "version"); !strings.Contains(out, "RuntimeName:  fun-with-sandboxes") || !strings.Contains(out, "RuntimeApiVersion:  v1") {
		t.Errorf("crictl version:\n%s", out)
	}
	if out := mustCrictl(t, "info"); !strings.Contains(out, `"RuntimeReady"`) {
		t.Errorf("crictl info:\n%s", out)
	}
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := l.Addr().(*net.TCPAddr).Port
	l.Close()
	pod := writeJSON(t, filepath.Join(dir, "pod.json"), map[string]any{
		"metadata":      map[string]any{"name": "web", "namespace": "e2e", "uid": "e2e-cri-web", "attempt": 0},
		"hostname":      "web",
		"log_directory": logs,
		"port_mappings": []map[string]any{{"protocol": 0, "container_port": 8080, "host_port": hostPort, "host_ip": "127.0.0.1"}},
		"labels":        map[string]string{"app": "web"},
		"linux":         map[string]any{},
	})
	sandbox := strings.TrimSpace(mustCrictl(t, "runp", pod))
	defer func() {
		crictl(t, "stopp", sandbox)
		crictl(t, "rmp", sandbox)
	}()
	var inspect struct {
		Status struct {
			State   string `json:"state"`
			Network struct {
				IP string `json:"ip"`
			} `json:"network"`
			Labels map[string]string `json:"labels"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(mustCrictl(t, "inspectp", "-o", "json", sandbox)), &inspect); err != nil {
		t.Fatal(err)
	}
	if inspect.Status.State != "SANDBOX_READY" || inspect.Status.Network.IP == "" || inspect.Status.Labels["app"] != "web" {
		t.Errorf("the sandbox's status: %+v", inspect.Status)
	}
	if out := mustCrictl(t, "pods", "--label", "app=web", "-q"); strings.TrimSpace(out) != sandbox {
		t.Errorf("crictl pods --label app=web: %q", out)
	}

	container := func(name string, args ...string) string {
		cfg := writeJSON(t, filepath.Join(dir, name+".json"), map[string]any{
			"metadata": map[string]any{"name": name},
			"image":    map[string]any{"image": "fws/toolbox:1"},
			"args":     args,
			"log_path": name + ".log",
			"envs":     []map[string]string{{"key": "SANDBOX_GREETING", "value": "hello"}},
			"linux":    map[string]any{},
		})
		id := strings.TrimSpace(mustCrictl(t, "create", sandbox, cfg, pod))
		mustCrictl(t, "start", id)
		return id
	}
	web := container("web", "serve")
	client := container("client", "hold")
	greeter := container("greeter", "echo", "from the pod")

	// The pod's containers reach one another on localhost.
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := crictl(t, "exec", "--sync", client, "/toolbox", "fetch", "http://127.0.0.1:8080/healthz")
		if err == nil && strings.Contains(out, "200") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the client never reached web on localhost: %v\n%s", err, out)
		}
		time.Sleep(time.Second)
	}
	// Another pod's sandbox reaches the pod at its address on the pod
	// network.
	if !netip.MustParsePrefix("10.244.0.0/16").Contains(netip.MustParseAddr(inspect.Status.Network.IP)) {
		t.Errorf("the sandbox's address %s is not on the pod network", inspect.Status.Network.IP)
	}
	peerPod := writeJSON(t, filepath.Join(dir, "peer.json"), map[string]any{
		"metadata": map[string]any{"name": "peer", "namespace": "e2e", "uid": "e2e-cri-peer", "attempt": 0},
		"hostname": "peer", "log_directory": logs, "linux": map[string]any{},
	})
	peerSandbox := strings.TrimSpace(mustCrictl(t, "runp", peerPod))
	defer func() {
		crictl(t, "stopp", peerSandbox)
		crictl(t, "rmp", peerSandbox)
	}()
	peerCfg := writeJSON(t, filepath.Join(dir, "peer-client.json"), map[string]any{
		"metadata": map[string]any{"name": "peer-client"},
		"image":    map[string]any{"image": "fws/toolbox:1"},
		"args":     []string{"hold"},
		"log_path": "peer-client.log",
		"linux":    map[string]any{},
	})
	peerClient := strings.TrimSpace(mustCrictl(t, "create", peerSandbox, peerCfg, peerPod))
	mustCrictl(t, "start", peerClient)
	if out, err := crictl(t, "exec", "--sync", peerClient, "/toolbox", "fetch", "http://"+inspect.Status.Network.IP+":8080/healthz"); err != nil || !strings.Contains(out, "200") {
		t.Errorf("another pod fetching the pod at %s: %v\n%s", inspect.Status.Network.IP, err, out)
	}
	mustCrictl(t, "stopp", peerSandbox)
	mustCrictl(t, "rmp", peerSandbox)

	res, err := http.Get("http://127.0.0.1:" + strconv.Itoa(hostPort) + "/healthz")
	if err != nil {
		t.Fatalf("the sandbox's published port: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("the sandbox's published port answered %s", res.Status)
	}
	if out := mustCrictl(t, "exec", "--sync", "--timeout", "10", client, "/toolbox", "env"); !strings.Contains("\n"+out, "\nSANDBOX_GREETING=hello\n") {
		t.Errorf("crictl exec --sync: %q", out)
	}
	if _, err := crictl(t, "exec", "--sync", client, "/toolbox", "fetch", "http://127.0.0.1:1/"); err == nil {
		t.Error("a failing exec's exit status was lost")
	}
	// The runtime's stream server runs a streamed exec, and forwards a
	// local port to the pod's.
	if out := mustCrictl(t, "exec", client, "/toolbox", "env"); !strings.Contains("\n"+out, "\nSANDBOX_GREETING=hello\n") {
		t.Errorf("crictl exec: %q", out)
	}
	if _, err := crictl(t, "exec", client, "/toolbox", "fetch", "http://127.0.0.1:1/"); err == nil {
		t.Error("a streamed exec's exit status was lost")
	}
	fl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	local := fl.Addr().(*net.TCPAddr).Port
	fl.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pf := exec.CommandContext(ctx, "crictl", "--runtime-endpoint", dockerHost, "port-forward", sandbox, strconv.Itoa(local)+":8080")
	pfOut, err := pf.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := pf.Start(); err != nil {
		t.Fatal(err)
	}
	defer pf.Wait()
	// crictl reports each local address it forwards from as it listens.
	buf := make([]byte, 4096)
	var seen strings.Builder
	for !strings.Contains(seen.String(), "Forwarding from") {
		n, err := pfOut.Read(buf)
		seen.Write(buf[:n])
		if err != nil {
			t.Fatalf("crictl port-forward ended: %v\n%s", err, seen.String())
		}
	}
	go io.Copy(io.Discard, pfOut)
	res, err = http.Get("http://127.0.0.1:" + strconv.Itoa(local) + "/healthz")
	if err != nil {
		t.Fatalf("a forwarded port: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("a forwarded port answered %s", res.Status)
	}
	cancel()

	// The greeter's output, in the file the client named, in CRI's format.
	// CRI has no call that waits for a container to exit; its clients poll
	// its status.
	var status struct {
		Status struct {
			State    string `json:"state"`
			ExitCode int    `json:"exitCode"`
			Reason   string `json:"reason"`
			LogPath  string `json:"logPath"`
		} `json:"status"`
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		if err := json.Unmarshal([]byte(mustCrictl(t, "inspect", "-o", "json", greeter)), &status); err != nil {
			t.Fatal(err)
		}
		if status.Status.State != "CONTAINER_RUNNING" || time.Now().After(deadline) {
			break
		}
	}
	if s := status.Status; s.State != "CONTAINER_EXITED" || s.ExitCode != 0 || s.Reason != "Completed" || s.LogPath != filepath.Join(logs, "greeter.log") {
		t.Errorf("the greeter's status: %+v", s)
	}
	if out := mustCrictl(t, "logs", greeter); out != "from the pod\n" {
		t.Errorf("crictl logs: %q", out)
	}
	b, err := os.ReadFile(filepath.Join(logs, "greeter.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT[0-9:.]+Z stdout F from the pod\n$`).Match(b) {
		t.Errorf("the greeter's CRI log: %q", b)
	}

	if out := mustCrictl(t, "ps", "-a", "--pod", sandbox, "-q"); strings.Count(out, "\n") != 3 {
		t.Errorf("crictl ps --pod: %q", out)
	}
	if out := mustCrictl(t, "stats", "-o", "json", web); !strings.Contains(out, `"usageCoreNanoSeconds"`) {
		t.Errorf("crictl stats:\n%s", out)
	}
	if out := mustCrictl(t, "statsp", "-o", "json", sandbox); !strings.Contains(out, `"processCount"`) {
		t.Errorf("crictl statsp:\n%s", out)
	}

	mustCrictl(t, "stopp", sandbox)
	mustCrictl(t, "rmp", sandbox)
	if out := mustCrictl(t, "ps", "-a", "-q"); strings.Contains(out, client) {
		t.Errorf("a removed sandbox's containers remain: %q", out)
	}
	if out := mustDocker(t, "ps", "-a", "--format", "{{.Names}}"); strings.Contains(out, "k8s_") {
		t.Errorf("the engine still holds the pod's containers:\n%s", out)
	}
}

// A CRI client pulls with the credentials it is given, lists, inspects
// and removes images.
func TestCRIImages(t *testing.T) {
	host := basicAuthRegistry(t, "fws", "s3cret")
	tag, _ := name.NewTag("fws/toolbox:1")
	base, err := tarball.ImageFromPath(filepath.Join(os.Getenv("FWS_E2E_IMAGES"), "toolbox.tar"), &tag)
	if err != nil {
		t.Fatal(err)
	}
	// An image of its own: removing an image removes every name its ID
	// has, and the suite runs fws/toolbox:1 throughout.
	cfg, err := base.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.DeepCopy()
	cfg.Config.Labels = map[string]string{"e2e": "cri"}
	img, err := mutate.ConfigFile(base, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/e2e/cri:1"
	pushed, _ := name.ParseReference(ref)
	if err := remote.Write(pushed, img, remote.WithAuth(&authn.Basic{Username: "fws", Password: "s3cret"})); err != nil {
		t.Fatal(err)
	}
	if _, err := crictl(t, "pull", ref); err == nil {
		t.Error("a pull without credentials succeeded")
	}
	mustCrictl(t, "pull", "--creds", "fws:s3cret", ref)
	if out := mustCrictl(t, "images", "-o", "json"); !strings.Contains(out, ref) {
		t.Errorf("crictl images:\n%s", out)
	}
	if out := mustCrictl(t, "inspecti", "-o", "json", ref); !strings.Contains(out, `"repoTags"`) {
		t.Errorf("crictl inspecti:\n%s", out)
	}
	if out := mustCrictl(t, "imagefsinfo", "-o", "json"); !strings.Contains(out, `"usedBytes"`) {
		t.Errorf("crictl imagefsinfo:\n%s", out)
	}
	mustCrictl(t, "rmi", ref)
	if _, err := crictl(t, "inspecti", ref); err == nil {
		t.Error("a removed image was inspected")
	}
	mustDocker(t, "image", "inspect", "fws/toolbox:1")
}
