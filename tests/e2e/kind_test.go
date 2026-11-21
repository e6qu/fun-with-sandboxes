//go:build e2e && darwin

package e2e

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gname "github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cmd"
	dockerimage "sigs.k8s.io/kind/pkg/cmd/kind/load/docker-image"
	imagearchive "sigs.k8s.io/kind/pkg/cmd/kind/load/image-archive"
	kindexec "sigs.k8s.io/kind/pkg/exec"
	"sigs.k8s.io/yaml"
)

// kind's docker provider creates a cluster of fws/kind-node nodes on the
// engine, as kind create cluster does: a control-plane node, which kubeadm
// init brings up with the control plane as static pods on the node's
// network, and a worker, which kubeadm join adds. The API server answers
// on the host's loopback port kind publishes, with the kubeconfig kind
// writes. kind load docker-image and image-archive put images in the
// nodes' engines; a Deployment's pods run on the worker, a pod reaches
// them through their Service by name, and the host through its node port,
// which the worker publishes as kind's extraPortMappings do. kind export logs collects the
// nodes' logs, and kind deletes the cluster.
func TestKindCluster(t *testing.T) {
	const name = "fws-e2e"
	provider := cluster.NewProvider(cluster.ProviderWithDocker(), cluster.ProviderWithLogger(cmd.NewLogger()))
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	nodes := []string{name + "-control-plane", name + "-worker"}
	defer kindCleanup(t, provider, name, kubeconfig, nodes)
	const nodePort = 30080
	cfg := &v1alpha4.Cluster{Nodes: []v1alpha4.Node{
		{Role: v1alpha4.ControlPlaneRole},
		{Role: v1alpha4.WorkerRole, ExtraPortMappings: []v1alpha4.PortMapping{{ContainerPort: nodePort, ListenAddress: "127.0.0.1"}}},
	}}
	if err := provider.Create(name,
		cluster.CreateWithV1Alpha4Config(cfg),
		cluster.CreateWithNodeImage("fws/kind-node:1.35"),
		cluster.CreateWithKubeconfigPath(kubeconfig),
		cluster.CreateWithRetain(true),
		cluster.CreateWithWaitForReady(3*time.Minute),
		cluster.CreateWithDisplayUsage(false), cluster.CreateWithDisplaySalutation(false),
	); err != nil {
		var run *kindexec.RunError
		if errors.As(err, &run) {
			t.Fatalf("kind create cluster: %v\n%s", err, tail(string(run.Output), 80))
		}
		t.Fatalf("kind create cluster: %v", err)
	}
	if got, err := provider.ListNodes(name); err != nil || len(got) != 2 {
		t.Fatalf("the cluster's nodes: %v, %v", got, err)
	}

	kubectl := func(args ...string) (string, error) {
		return docker(t, append([]string{"exec", nodes[0], "kubectl"}, args...)...)
	}
	mustKubectl := func(args ...string) string {
		t.Helper()
		out, err := kubectl(args...)
		if err != nil {
			t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	for _, n := range nodes {
		mustKubectl("wait", "--for=condition=Ready", "--timeout=2m", "node/"+n)
	}
	if out := mustKubectl("get", "node", nodes[1], "-o", "jsonpath={.status.nodeInfo.operatingSystem} {.status.nodeInfo.kubeletVersion} {.spec.providerID}"); out != "darwin v1.35.0 kind://docker/"+name+"/"+nodes[1] {
		t.Errorf("the worker's info: %q", out)
	}
	// The control plane runs as kubeadm's static pods, on the node's
	// network, at the node's address on kind's network.
	nodeIP := strings.TrimSpace(mustDocker(t, "inspect", "-f", `{{(index .NetworkSettings.Networks "kind").IPAddress}}`, nodes[0]))
	for _, c := range []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
		pod := c + "-" + nodes[0]
		mustKubectl("wait", "--for=condition=Ready", "--timeout=2m", "-n", "kube-system", "pod/"+pod)
		if out := mustKubectl("get", "-n", "kube-system", "pod/"+pod, "-o", "jsonpath={.spec.hostNetwork} {.status.podIP}"); out != "true "+nodeIP || strings.HasPrefix(nodeIP, "127.") {
			t.Errorf("%s: %q", pod, out)
		}
	}

	// The API server answers on the host, at the port kind published and
	// with the credentials of the kubeconfig kind wrote.
	if out, err := apiGet(kubeconfig, "/api/v1/nodes"); err != nil || !strings.Contains(out, nodes[1]) {
		t.Errorf("the API server from the host: %v\n%s", err, tail(out, 10))
	}

	// kind load docker-image puts the images in each node's engine, through
	// the node's ctr, and finds them there by crictl the second time.
	for range 2 {
		load := dockerimage.NewCommand(cmd.NewLogger(), cmd.StandardIOStreams())
		load.SetArgs([]string{"--name", name, "fws/nginx:1", "fws/toolbox:1"})
		if err := load.Execute(); err != nil {
			var run *kindexec.RunError
			if errors.As(err, &run) {
				t.Fatalf("kind load docker-image: %v\n%s", err, tail(string(run.Output), 40))
			}
			t.Fatalf("kind load docker-image: %v", err)
		}
	}
	// kind load image-archive does the same from an archive docker save
	// wrote.
	archive := filepath.Join(t.TempDir(), "busybox.tar")
	mustDocker(t, "save", "-o", archive, "fws/busybox:1")
	loadArchive := imagearchive.NewCommand(cmd.NewLogger(), cmd.StandardIOStreams())
	loadArchive.SetArgs([]string{"--name", name, archive})
	if err := loadArchive.Execute(); err != nil {
		t.Fatalf("kind load image-archive: %v", err)
	}
	for _, n := range nodes {
		for _, img := range []string{"fws/nginx:1", "fws/busybox:1"} {
			if out, err := docker(t, "exec", n, "crictl", "inspecti", img); err != nil || !strings.Contains(out, `"repoTags"`) {
				t.Errorf("crictl inspecti %s in %s: %v\n%s", img, n, err, tail(out, 10))
			}
		}
	}
	mustKubectl("create", "deployment", "web", "--image=fws/nginx:1", "--replicas=2")
	mustKubectl("rollout", "status", "--timeout=3m", "deployment/web")
	if out := mustKubectl("get", "pods", "-l", "app=web", "-o", "jsonpath={range .items[*]}{.spec.nodeName} {end}"); strings.TrimSpace(out) != nodes[1]+" "+nodes[1] {
		t.Errorf("the Deployment's pods ran on %q", out)
	}
	mustKubectl("create", "service", "nodeport", "web", "--tcp=80:80", fmt.Sprintf("--node-port=%d", nodePort))
	// The fetch restarts until fws-kube-proxy has passed the Service on.
	mustKubectl("run", "fetcher", "--image=fws/toolbox:1", "--restart=OnFailure", "--", "fetch", "http://web.default.svc.cluster.local/")
	if _, err := kubectl("wait", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=2m", "pod/fetcher"); err != nil {
		out, _ := kubectl("logs", "fetcher")
		t.Errorf("a pod fetching the Service: %v\n%s", err, out)
	} else if out := mustKubectl("logs", "fetcher"); out != "200 OK\n" {
		t.Errorf("a pod fetching the Service: %q", out)
	}
	// A pod's memory limit holds: the kubelet sends it, and a container
	// past it is killed, as under a memory cgroup.
	mustKubectl("run", "hog", "--image=fws/toolbox:1", "--restart=Never", "--overrides",
		`{"spec":{"containers":[{"name":"hog","image":"fws/toolbox:1","args":["alloc","512"],"resources":{"limits":{"memory":"32Mi","cpu":"500m"}}}]}}`)
	if _, err := kubectl("wait", "--for=jsonpath={.status.containerStatuses[0].state.terminated.reason}=OOMKilled", "--timeout=2m", "pod/hog"); err != nil {
		out, _ := kubectl("get", "pod", "hog", "-o", "jsonpath={.status}")
		t.Errorf("a pod past its memory limit: %v\n%s", err, out)
	}
	if out := mustKubectl("get", "storageclass", "standard", "-o", "jsonpath={.provisioner}"); out != "kubernetes.io/host-path" {
		t.Errorf("the default storage class: %q", out)
	}

	// The node port answers on the host, at the port kind published for
	// the worker's.
	hostAddr := strings.TrimSpace(mustDocker(t, "port", nodes[1], fmt.Sprintf("%d/tcp", nodePort)))
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(2 * time.Second) {
		res, err := http.Get("http://" + hostAddr + "/")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
			err = errors.New(res.Status)
		}
		if time.Now().After(deadline) {
			t.Errorf("the node port from the host at %s: %v", hostAddr, err)
			break
		}
	}

	// kind export logs: each node's journal, the kubelet's log, its images
	// by crictl and its /var/log.
	for _, n := range nodes {
		if out, err := docker(t, "exec", n, "crictl", "images"); err != nil || !strings.Contains(out, "fws/nginx") {
			t.Errorf("crictl images in %s: %v\n%s", n, err, tail(out, 20))
		}
	}
	logs := t.TempDir()
	if err := provider.CollectLogs(name, logs); err != nil {
		t.Errorf("kind export logs: %v", err)
		for _, n := range nodes {
			for _, f := range []string{"images.log", "journal.log", "kubelet.log"} {
				b, _ := os.ReadFile(filepath.Join(logs, n, f))
				t.Logf("%s's exported %s:\n%s", n, f, tail(string(b), 15))
			}
		}
	}
	for file, want := range map[string]string{
		"kubelet.log":            "kubelet",
		"journal.log":            "Reached target Multi-User System",
		"images.log":             "fws/nginx",
		"kubernetes-version.txt": "v1.35.0",
		"serial.log":             "Reached target Multi-User System",
	} {
		b, err := os.ReadFile(filepath.Join(logs, nodes[1], file))
		if err != nil || !strings.Contains(string(b), want) {
			t.Errorf("kind export logs: %s's %s: %v\n%s", nodes[1], file, err, tail(string(b), 10))
		}
	}
	if pods, _ := filepath.Glob(filepath.Join(logs, nodes[1], "pods", "default_web-*")); len(pods) != 2 {
		t.Errorf("kind export logs: the worker's pod logs: %q", pods)
	}
}

// apiGet fetches path from the API server of kubeconfig's current
// cluster, with its user's client certificate.
func apiGet(kubeconfig, path string) (string, error) {
	b, err := os.ReadFile(kubeconfig)
	if err != nil {
		return "", err
	}
	var kc struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CA     string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct {
				Cert string `json:"client-certificate-data"`
				Key  string `json:"client-key-data"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal(b, &kc); err != nil {
		return "", err
	}
	if len(kc.Clusters) != 1 || len(kc.Users) != 1 {
		return "", fmt.Errorf("a kubeconfig of %d clusters and %d users", len(kc.Clusters), len(kc.Users))
	}
	decode := func(s string) []byte { d, _ := base64.StdEncoding.DecodeString(s); return d }
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(decode(kc.Clusters[0].Cluster.CA))
	cert, err := tls.X509KeyPair(decode(kc.Users[0].User.Cert), decode(kc.Users[0].User.Key))
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}}}}
	res, err := client.Get(kc.Clusters[0].Cluster.Server + path)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return string(body), fmt.Errorf("%s: %s", path, res.Status)
	}
	return string(body), nil
}

// kindCleanup deletes kind cluster name, whose nodes are nodes, and kind's
// network, logging the nodes' state, logs and workloads first when the
// test failed.
func kindCleanup(t *testing.T, provider *cluster.Provider, name, kubeconfig string, nodes []string) {
	if t.Failed() {
		for _, n := range nodes {
			state, _ := docker(t, "inspect", "-f", "{{json .State}}", n)
			out, _ := exec.Command("docker", "logs", n).CombinedOutput()
			t.Logf("%s: %s\n%s", n, strings.TrimSpace(state), tail(string(out), 80))
		}
		for _, args := range [][]string{
			{"get", "nodes", "-o", "wide"},
			{"get", "pods", "-A", "-o", "wide"},
			{"describe", "pods", "-A"},
			{"get", "events", "-A"},
		} {
			out, _ := docker(t, append([]string{"exec", nodes[0], "kubectl"}, args...)...)
			t.Logf("kubectl %s:\n%s", strings.Join(args, " "), tail(out, 120))
		}
	}
	// A delete that never ends stops the suite; fwsd's goroutines,
	// which SIGQUIT writes to its log, show where it waits.
	deleted := make(chan error, 1)
	go func() { deleted <- provider.Delete(name, kubeconfig) }()
	select {
	case err := <-deleted:
		if err != nil {
			t.Errorf("kind delete cluster: %v", err)
		}
	case <-time.After(5 * time.Minute):
		t.Errorf("kind delete cluster did not end in 5 minutes; fwsd's goroutines follow in its log")
		exec.Command("pkill", "-QUIT", "-f", fwsdBin).Run()
		time.Sleep(10 * time.Second)
		return
	}
	for _, n := range nodes {
		if out, err := docker(t, "inspect", n); err == nil {
			t.Errorf("kind delete cluster left %s:\n%s", n, tail(out, 5))
		}
	}
	docker(t, "network", "rm", "kind")
}

// IPv6 and dual-stack kind clusters, kind's ipFamily ipv6 and dual: each
// node takes an IPv6 address on kind's network, and an IPv4 one in a
// dual-stack cluster, and its pod network holds the pod CIDR the
// controller manager gives it, of each family, IPv6-only in an IPv6
// cluster. A pod on the control-plane node reaches a pod on the worker,
// another node's, at its IPv6 address and through its Service.
func TestKindIPv6AndDualStackClusters(t *testing.T) {
	for _, family := range []v1alpha4.ClusterIPFamily{v1alpha4.IPv6Family, v1alpha4.DualStackFamily} {
		t.Run(string(family), func(t *testing.T) { kindFamilyCluster(t, family) })
	}
}

func kindFamilyCluster(t *testing.T, family v1alpha4.ClusterIPFamily) {
	name := "fws-e2e-" + string(family)
	provider := cluster.NewProvider(cluster.ProviderWithDocker(), cluster.ProviderWithLogger(cmd.NewLogger()))
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	nodes := []string{name + "-control-plane", name + "-worker"}
	defer kindCleanup(t, provider, name, kubeconfig, nodes)
	cfg := &v1alpha4.Cluster{
		Networking: v1alpha4.Networking{IPFamily: family},
		Nodes:      []v1alpha4.Node{{Role: v1alpha4.ControlPlaneRole}, {Role: v1alpha4.WorkerRole}},
	}
	if err := provider.Create(name,
		cluster.CreateWithV1Alpha4Config(cfg),
		cluster.CreateWithNodeImage("fws/kind-node:1.35"),
		cluster.CreateWithKubeconfigPath(kubeconfig),
		cluster.CreateWithRetain(true),
		cluster.CreateWithWaitForReady(3*time.Minute),
		cluster.CreateWithDisplayUsage(false), cluster.CreateWithDisplaySalutation(false),
	); err != nil {
		var run *kindexec.RunError
		if errors.As(err, &run) {
			t.Fatalf("kind create cluster: %v\n%s", err, tail(string(run.Output), 80))
		}
		t.Fatalf("kind create cluster: %v", err)
	}
	mustKubectl := func(args ...string) string {
		t.Helper()
		out, err := docker(t, append([]string{"exec", nodes[0], "kubectl"}, args...)...)
		if err != nil {
			t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	// A family's addresses, of a space-separated list: IPv6 alone in an
	// IPv6 cluster, one of each in a dual-stack one.
	families := func(list string) string {
		var v4, v6 int
		for _, a := range strings.Fields(list) {
			if strings.Contains(a, ":") {
				v6++
			} else {
				v4++
			}
		}
		return fmt.Sprintf("%d IPv4 %d IPv6", v4, v6)
	}
	want := "0 IPv4 1 IPv6"
	if family == v1alpha4.DualStackFamily {
		want = "1 IPv4 1 IPv6"
	}
	for _, n := range nodes {
		mustKubectl("wait", "--for=condition=Ready", "--timeout=2m", "node/"+n)
		ips := mustKubectl("get", "node", n, "-o", `jsonpath={range .status.addresses[?(@.type=="InternalIP")]}{.address} {end}`)
		cidrs := mustKubectl("get", "node", n, "-o", `jsonpath={range .spec.podCIDRs[*]}{@} {end}`)
		if families(ips) != want || families(cidrs) != want {
			t.Errorf("%s's addresses %q and pod CIDRs %q, want %s of each", n, ips, cidrs, want)
		}
	}
	load := dockerimage.NewCommand(cmd.NewLogger(), cmd.StandardIOStreams())
	load.SetArgs([]string{"--name", name, "fws/nginx:1", "fws/toolbox:1"})
	if err := load.Execute(); err != nil {
		t.Fatalf("kind load docker-image: %v", err)
	}
	mustKubectl("create", "deployment", "web", "--image=fws/nginx:1")
	mustKubectl("rollout", "status", "--timeout=3m", "deployment/web")
	on := mustKubectl("get", "pods", "-l", "app=web", "-o", "jsonpath={.items[0].spec.nodeName}")
	ips := mustKubectl("get", "pods", "-l", "app=web", "-o", "jsonpath={range .items[0].status.podIPs[*]}{.ip} {end}")
	if on != nodes[1] || families(ips) != want {
		t.Fatalf("the web pod on %q at %q, want one on %s with %s", on, ips, nodes[1], want)
	}
	var ip6 string
	for _, a := range strings.Fields(ips) {
		if strings.Contains(a, ":") {
			ip6 = a
		}
	}
	mustKubectl("expose", "deployment", "web", "--port=80")
	// The fetchers run on the control-plane node, whose pods reach the
	// worker's across nodes; each restarts until it gets through.
	for i, url := range []string{"http://[" + ip6 + "]/", "http://web.default.svc.cluster.local/"} {
		fetcher := fmt.Sprintf("fetcher-%d", i)
		mustKubectl("run", fetcher, "--image=fws/toolbox:1", "--restart=OnFailure",
			`--overrides={"spec":{"nodeName":"`+nodes[0]+`"}}`, "--", "fetch", url)
		if _, err := docker(t, "exec", nodes[0], "kubectl", "wait", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=2m", "pod/"+fetcher); err != nil {
			out, _ := docker(t, "exec", nodes[0], "kubectl", "logs", fetcher)
			t.Errorf("a pod on %s fetching %s: %v\n%s", nodes[0], url, err, out)
		} else if out := mustKubectl("logs", fetcher); out != "200 OK\n" {
			t.Errorf("a pod on %s fetching %s: %q", nodes[0], url, out)
		}
	}
}

// kind's local registry, as kind's documentation sets one up: a registry
// container, a cluster whose containerdConfigPatches name a hosts
// directory, and on each node a hosts.toml that sends a registry's pulls
// to the registry container, which joins kind's network. A pod's image of
// a registry that does not exist pulls through it, as the node's runtime
// reads the patched configuration, and reaches the registry container by
// its name there.
func TestKindLocalRegistry(t *testing.T) {
	const name, registryName, upstream = "fws-e2e-reg", "fws-e2e-kind-registry", "registry.fws-e2e.invalid"
	provider := cluster.NewProvider(cluster.ProviderWithDocker(), cluster.ProviderWithLogger(cmd.NewLogger()))
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	nodes := []string{name + "-control-plane", name + "-worker"}
	defer kindCleanup(t, provider, name, kubeconfig, nodes)
	mustDocker(t, "run", "-d", "--name", registryName, "-p", "127.0.0.1::5000", "fws/toolbox:1", "registry", "0.0.0.0:5000")
	// Before kindCleanup, which removes kind's network.
	defer docker(t, "rm", "-f", registryName)
	hostAddr := strings.TrimSpace(mustDocker(t, "port", registryName, "5000/tcp"))
	tag, _ := gname.NewTag("fws/toolbox:1")
	img, err := tarball.ImageFromPath(filepath.Join(os.Getenv("FWS_E2E_IMAGES"), "toolbox.tar"), &tag)
	if err != nil {
		t.Fatal(err)
	}
	pushed, _ := gname.ParseReference(hostAddr + "/e2e/toolbox:1")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		if err = remote.Write(pushed, img); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("pushing to the registry container: %v", err)
	}

	cfg := &v1alpha4.Cluster{
		ContainerdConfigPatches: []string{`[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d"`},
		Nodes: []v1alpha4.Node{{Role: v1alpha4.ControlPlaneRole}, {Role: v1alpha4.WorkerRole}},
	}
	if err := provider.Create(name,
		cluster.CreateWithV1Alpha4Config(cfg),
		cluster.CreateWithNodeImage("fws/kind-node:1.35"),
		cluster.CreateWithKubeconfigPath(kubeconfig),
		cluster.CreateWithRetain(true),
		cluster.CreateWithWaitForReady(3*time.Minute),
		cluster.CreateWithDisplayUsage(false), cluster.CreateWithDisplaySalutation(false),
	); err != nil {
		var run *kindexec.RunError
		if errors.As(err, &run) {
			t.Fatalf("kind create cluster: %v\n%s", err, tail(string(run.Output), 80))
		}
		t.Fatalf("kind create cluster: %v", err)
	}
	mustDocker(t, "network", "connect", "kind", registryName)
	hosts := fmt.Sprintf("[host.\"http://%s:5000\"]\n  capabilities = [\"pull\", \"resolve\"]\n", registryName)
	for _, n := range nodes {
		dir := "/etc/containerd/certs.d/" + upstream
		mustDocker(t, "exec", n, "sh", "-c", `mkdir -p "$1" && printf '%s' "$2" > "$1/hosts.toml"`, "sh", dir, hosts)
	}
	mustKubectl := func(args ...string) string {
		t.Helper()
		out, err := docker(t, append([]string{"exec", nodes[0], "kubectl"}, args...)...)
		if err != nil {
			t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	for _, n := range nodes {
		mustKubectl("wait", "--for=condition=Ready", "--timeout=2m", "node/"+n)
	}
	mustKubectl("run", "puller", "--image="+upstream+"/e2e/toolbox:1", "--restart=Never", "--", "echo", "pulled through the mirror")
	if _, err := docker(t, "exec", nodes[0], "kubectl", "wait", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=3m", "pod/puller"); err != nil {
		out, _ := docker(t, "exec", nodes[0], "kubectl", "describe", "pod", "puller")
		t.Fatalf("the pod of %s's image: %v\n%s", upstream, err, tail(out, 40))
	}
	if out := mustKubectl("logs", "puller"); strings.TrimSpace(out) != "pulled through the mirror" {
		t.Errorf("the pod printed %q", out)
	}
}
