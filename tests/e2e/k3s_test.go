//go:build e2e && darwin

package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/modules/k3s"
)

// Testcontainers' k3s module runs fws/k3s as it runs rancher/k3s: on the
// default bridge network, whose address the node takes, k3s server comes
// up and logs what the module waits for; the kubeconfig the module reads
// reaches the API server on the host; LoadImages puts an image in the
// node's engine through its ctr; a pod of it runs on the node, and
// another reaches it through its Service by name.
func TestK3sModule(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c, err := k3s.Run(ctx, "fws/k3s:1.37")
	defer func() {
		if c != nil && t.Failed() {
			if r, err := c.Logs(ctx); err == nil {
				b, _ := io.ReadAll(r)
				t.Logf("the node's log:\n%s", tail(string(b), 80))
			}
		}
		testcontainers.CleanupContainer(t, c)
	}()
	if err != nil {
		t.Fatalf("k3s.Run: %v", err)
	}
	kubeconfig, err := c.GetKubeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := apiGet(path, "/api/v1/nodes"); err != nil || !strings.Contains(out, `"operatingSystem":"darwin"`) {
		t.Fatalf("the API server from the host: %v\n%s", err, tail(out, 10))
	}
	kubectl := func(args ...string) (string, error) {
		code, r, err := c.Exec(ctx, append([]string{"k3s", "kubectl"}, args...), tcexec.Multiplexed())
		if err != nil {
			return "", err
		}
		b, _ := io.ReadAll(r)
		if code != 0 {
			return string(b), fmt.Errorf("exit status %d", code)
		}
		return string(b), nil
	}
	if out, err := kubectl("get", "nodes", "-o", "jsonpath={.items[0].status.addresses[0].address}"); err != nil || !strings.HasPrefix(out, "172.17.") {
		t.Errorf("the node's address: %v %q", err, out)
	}
	if err := c.LoadImages(ctx, "fws/nginx:1", "fws/toolbox:1"); err != nil {
		t.Fatalf("LoadImages: %v", err)
	}
	if out, err := kubectl("run", "web", "--image=fws/nginx:1", "--image-pull-policy=Never"); err != nil {
		t.Fatalf("kubectl run: %v\n%s", err, out)
	}
	if out, err := kubectl("wait", "--for=condition=Ready", "--timeout=3m", "pod/web"); err != nil {
		desc, _ := kubectl("describe", "pod", "web")
		t.Fatalf("the pod: %v\n%s\n%s", err, out, tail(desc, 40))
	}
	// A Service in front of the pod, which another pod reaches by its DNS
	// name, and whose answer the first pod's log records.
	if out, err := kubectl("expose", "pod", "web", "--port=80"); err != nil {
		t.Fatalf("kubectl expose: %v\n%s", err, out)
	}
	if out, err := kubectl("run", "fetcher", "--image=fws/toolbox:1", "--image-pull-policy=Never", "--restart=OnFailure", "--", "fetch", "http://web.default.svc.cluster.local/"); err != nil {
		t.Fatalf("kubectl run: %v\n%s", err, out)
	}
	if _, err := kubectl("wait", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=2m", "pod/fetcher"); err != nil {
		out, _ := kubectl("logs", "fetcher")
		t.Errorf("a pod fetching the Service by name: %v\n%s", err, out)
	} else if out, _ := kubectl("logs", "fetcher"); out != "200 OK\n" {
		t.Errorf("a pod fetching the Service by name: %q", out)
	}
	if out, err := kubectl("logs", "web"); err != nil || !strings.Contains(out, `"GET / HTTP/1.1" 200`) {
		t.Errorf("kubectl logs of the served pod: %v\n%s", err, tail(out, 10))
	}
}
