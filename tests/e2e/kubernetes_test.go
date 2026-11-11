//go:build e2e && darwin

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pki writes the certificates and kubeconfigs a control plane runs with,
// as kubeadm's certs and kubeconfig phases lay them out under dir.
func pki(t *testing.T, dir, server string) {
	t.Helper()
	key := func() *rsa.PrivateKey {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	write := func(name string, typ string, der []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	serial := int64(1)
	now := time.Now()
	caKey := key()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "kubernetes"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		IsCA:     true, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	write("ca.crt", "CERTIFICATE", caDER)
	write("ca.key", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(caKey))
	issue := func(name string, tmpl *x509.Certificate) {
		serial++
		k := key()
		tmpl.SerialNumber = big.NewInt(serial)
		tmpl.NotBefore, tmpl.NotAfter = now.Add(-time.Hour), now.Add(24*time.Hour)
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		write(name+".crt", "CERTIFICATE", der)
		write(name+".key", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(k))
	}
	issue("apiserver", &x509.Certificate{
		Subject:     pkix.Name{CommonName: "kube-apiserver"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"kube-apiserver", "kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local", "localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.96.0.1")},
	})
	sa := key()
	write("sa.key", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(sa))
	pub, err := x509.MarshalPKIXPublicKey(&sa.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	write("sa.pub", "PUBLIC KEY", pub)
	caPEM, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
	for _, c := range []struct{ file, user, group string }{
		{"admin", "kubernetes-admin", "system:masters"},
		{"controller-manager", "system:kube-controller-manager", ""},
		{"scheduler", "system:kube-scheduler", ""},
		{"node1", "system:node:node1", "system:nodes"},
		{"node2", "system:node:node2", "system:nodes"},
		// kube-proxy's user, whom the bootstrap policy's
		// system:node-proxier binding lets watch services and endpoints.
		{"kube-proxy", "system:kube-proxy", ""},
	} {
		subject := pkix.Name{CommonName: c.user}
		if c.group != "" {
			subject.Organization = []string{c.group}
		}
		issue(c.file, &x509.Certificate{Subject: subject, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		crt, _ := os.ReadFile(filepath.Join(dir, c.file+".crt"))
		k, _ := os.ReadFile(filepath.Join(dir, c.file+".key"))
		conf := "apiVersion: v1\nkind: Config\nclusters:\n- name: kubernetes\n  cluster:\n    server: " + server +
			"\n    certificate-authority-data: " + base64.StdEncoding.EncodeToString(caPEM) +
			"\ncontexts:\n- name: default\n  context:\n    cluster: kubernetes\n    user: " + c.user +
			"\ncurrent-context: default\nusers:\n- name: " + c.user +
			"\n  user:\n    client-certificate-data: " + base64.StdEncoding.EncodeToString(crt) + "\n    client-key-data: " + base64.StdEncoding.EncodeToString(k) + "\n"
		if err := os.WriteFile(filepath.Join(dir, c.file+".conf"), []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Kubernetes' control plane runs in containers, as kubeadm's static pods
// run it: etcd, the API server, the controller manager and the scheduler
// on one network. The controller manager turns a Deployment into its
// Pods, the scheduler finds no node for them, and both hold their leases.
// Then a kubelet joins in a privileged container and runs the Pods on the
// engine nested in it, through the Container Runtime Interface the engine
// serves there, and the API server reads their logs from it.
func TestKubernetesControlPlane(t *testing.T) {
	const network = "e2e-k8s"
	dir := t.TempDir()
	pki(t, dir, "https://kube-apiserver:6443")
	// The API server advertises an address of its interfaces, which only
	// a network with a subnet of its own gives out: as kind does, the
	// network names one.
	mustDocker(t, "network", "create", "--subnet", "172.30.7.0/24", network)
	defer docker(t, "network", "rm", network)
	run := func(name string, args ...string) {
		t.Helper()
		mustDocker(t, append([]string{"run", "-d", "--restart=on-failure", "--name", "e2e-" + name, "--network", network, "--network-alias", name,
			"-v", dir + ":/etc/kubernetes/pki:ro"}, args...)...)
	}
	names := []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kubelet", "kubelet2"}
	defer func() {
		if t.Failed() {
			for _, n := range names {
				state, _ := docker(t, "inspect", "-f", "{{json .State}} restarts={{.RestartCount}}", "e2e-"+n)
				// docker logs writes the container's stderr to its own.
				out, _ := exec.Command("docker", "logs", "e2e-"+n).CombinedOutput()
				t.Logf("%s: %s\n%s", n, strings.TrimSpace(state), tail(string(out), 40))
			}
		}
		for _, n := range names {
			docker(t, "rm", "-f", "e2e-"+n)
		}
	}()
	run("etcd", "fws/etcd:3.6", "etcd", "--name", "etcd", "--data-dir", "/var/lib/etcd/data",
		"--listen-client-urls", "http://0.0.0.0:2379", "--advertise-client-urls", "http://etcd:2379")
	const pkiDir = "/etc/kubernetes/pki/"
	run("kube-apiserver", "fws/kube-apiserver:1.35", "kube-apiserver",
		"--etcd-servers=http://etcd:2379", "--secure-port=6443",
		"--tls-cert-file="+pkiDir+"apiserver.crt", "--tls-private-key-file="+pkiDir+"apiserver.key",
		"--client-ca-file="+pkiDir+"ca.crt",
		"--service-account-issuer=https://kubernetes.default.svc.cluster.local",
		"--service-account-key-file="+pkiDir+"sa.pub", "--service-account-signing-key-file="+pkiDir+"sa.key",
		"--service-cluster-ip-range=10.96.0.0/12", "--authorization-mode=Node,RBAC",
		"--enable-admission-plugins=NodeRestriction", "--allow-privileged=true")
	kubectl := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", append([]string{"run", "--rm", "--network", network,
			"-v", dir + ":/etc/kubernetes/pki:ro", "fws/kubectl:1.35", "kubectl", "--kubeconfig=" + pkiDir + "admin.conf"}, args...)...)
		// The image's entrypoint, go-runner, logs to stderr.
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return string(out) + stderr.String(), err
		}
		return string(out), nil
	}
	// nodeSock is node1's engine's socket, once node1 joins.
	var nodeSock string
	// On a failure, the pods as the API server has them and their
	// containers' logs, the last run's of a container that restarted.
	defer func() {
		if !t.Failed() {
			return
		}
		if nodeSock != "" {
			// What node1's engine holds, and nginx run on it outside a pod,
			// on the pod network and in a pod's network, to tell where a
			// pod's container fails.
			node := func(args ...string) string {
				out, err := docker(t, append([]string{"-H", "unix://" + nodeSock}, args...)...)
				if err != nil {
					return out + err.Error()
				}
				return out
			}
			ps := node("ps", "-a", "--format", "{{.ID}} {{.Names}} {{.Status}} {{.Ports}}")
			t.Logf("node1's containers:\n%s", ps)
			var pause string
			for _, l := range strings.Split(ps, "\n") {
				f := strings.Fields(l)
				if len(f) < 2 || !(strings.HasPrefix(f[1], "k8s_nginx") || strings.HasPrefix(f[1], "k8s_POD_web")) {
					continue
				}
				if strings.HasPrefix(f[1], "k8s_POD_web") && pause == "" {
					pause = f[0]
				}
				t.Logf("%s: %s", f[1], node("inspect", "-f", "exposed={{json .Config.ExposedPorts}} mode={{.HostConfig.NetworkMode}} ports={{json .NetworkSettings.Ports}} bindings={{json .HostConfig.PortBindings}}", f[0]))
			}
			for name, args := range map[string][]string{
				"bridge": {},
				"pods":   {"--network", "fws-pods"},
				"in-pod": {"--network", "container:" + pause},
			} {
				if name == "in-pod" && pause == "" {
					continue
				}
				c := "e2e-probe-nginx-" + name
				node(append(append([]string{"run", "-d", "--name", c}, args...), "fws/nginx:1")...)
				time.Sleep(5 * time.Second)
				// nginx logs to stderr, which docker logs writes to its own.
				logs, _ := exec.Command("docker", "-H", "unix://"+nodeSock, "logs", c).CombinedOutput()
				t.Logf("nginx on node1's engine, %s: %s\n%s", name, node("inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", c), tail(string(logs), 10))
				node("rm", "-f", c)
			}
		}
		out, _ := kubectl("get", "pods", "-A", "-o", "wide")
		t.Logf("pods:\n%s", out)
		names, _ := kubectl("get", "pods", "-o", "jsonpath={range .items[*]}{.metadata.name} {end}")
		for _, p := range strings.Fields(names) {
			prev, _ := kubectl("logs", "--previous", "--all-containers", p)
			cur, _ := kubectl("logs", "--all-containers", p)
			t.Logf("%s: previous:\n%s\ncurrent:\n%s", p, tail(prev, 30), tail(cur, 30))
		}
		out, _ = kubectl("describe", "pods", "-l", "app=web")
		t.Logf("web pods:\n%s", tail(out, 80))
	}()
	mustKubectl := func(args ...string) string {
		t.Helper()
		out, err := kubectl(args...)
		if err != nil {
			t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	awaitLog(t, "e2e-kube-apiserver", "Serving securely on")
	// The API server reports ready once its post-start hooks have run;
	// /readyz is how its clients learn that.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := kubectl("get", "--raw=/readyz")
		if err == nil && out == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the API server never became ready: %v\n%s", err, out)
		}
		time.Sleep(time.Second)
	}
	// As kubeadm's static pods, which the kubelet restarts, the controller
	// manager and the scheduler start against a serving API server and
	// restart while its bootstrap roles are being written.
	run("kube-controller-manager", "fws/kube-controller-manager:1.35", "kube-controller-manager",
		"--kubeconfig="+pkiDir+"controller-manager.conf",
		"--authentication-kubeconfig="+pkiDir+"controller-manager.conf", "--authorization-kubeconfig="+pkiDir+"controller-manager.conf",
		"--bind-address=127.0.0.1", "--leader-elect=true", "--use-service-account-credentials=true",
		"--service-account-private-key-file="+pkiDir+"sa.key", "--root-ca-file="+pkiDir+"ca.crt",
		"--cluster-signing-cert-file="+pkiDir+"ca.crt", "--cluster-signing-key-file="+pkiDir+"ca.key",
		"--controllers=*,bootstrapsigner,tokencleaner",
		// As kind's: each node takes a /24 of the cluster's pod subnet,
		// which its runtime's pod network takes as its own.
		"--allocate-node-cidrs=true", "--cluster-cidr=10.244.0.0/16")
	run("kube-scheduler", "fws/kube-scheduler:1.35", "kube-scheduler",
		"--kubeconfig="+pkiDir+"scheduler.conf",
		"--authentication-kubeconfig="+pkiDir+"scheduler.conf", "--authorization-kubeconfig="+pkiDir+"scheduler.conf",
		"--bind-address=127.0.0.1", "--leader-elect=true")

	if out := mustKubectl("version", "-o", "json"); !strings.Contains(out, `"gitVersion": "v1.35.0"`) {
		t.Errorf("kubectl version: %s", out)
	}

	for _, l := range []string{"kube-controller-manager", "kube-scheduler"} {
		mustKubectl("wait", "--for=create", "--timeout=2m", "-n", "kube-system", "lease/"+l)
		if out := mustKubectl("get", "-n", "kube-system", "lease/"+l, "-o", "jsonpath={.spec.holderIdentity}"); out == "" {
			t.Errorf("no one holds %s's lease", l)
		}
	}

	mustKubectl("wait", "--for=create", "--timeout=2m", "serviceaccount/default")
	if out := mustKubectl("create", "token", "default"); strings.Count(strings.TrimSpace(out), ".") != 2 {
		t.Errorf("a service account token: %q", out)
	}

	mustKubectl("create", "deployment", "web", "--image=fws/nginx:1", "--replicas=2")
	mustKubectl("wait", "--for=jsonpath={.status.replicas}=2", "--timeout=2m", "deployment/web")
	mustKubectl("wait", "--for=condition=PodScheduled=false", "--timeout=2m", "pod", "-l", "app=web")
	if out := mustKubectl("get", "events", "--field-selector", "reason=FailedScheduling", "-o", "jsonpath={.items[*].reportingComponent}"); !strings.Contains(out, "default-scheduler") {
		t.Errorf("the scheduler's events: %q", out)
	}

	// The node registers tainted, as kind's nodes take images it loads
	// before they run pods; the taint goes once the node's engine holds
	// the sandbox's pause image and the Deployment's. A --resolv-conf
	// with no lines passes the pods no DNS servers, which the engine's
	// containers cannot reach: they resolve names through the host's
	// resolver. An empty --resolv-conf would not: the kubelet then gives
	// each pod 127.0.0.1 and the search domain ".".
	// joinNode starts a node's kubelet, and fws-kube-proxy beside it,
	// which keeps the node engine's services to the cluster's as
	// kube-proxy keeps a node's; loads the images its pods run into the
	// node's engine; and lifts its taint. It returns the socket of the
	// node's engine.
	joinNode := func(node, container string) string {
		t.Helper()
		mustDocker(t, "run", "-d", "--privileged", "--restart=on-failure", "--name", container, "--network", network, "--network-alias", node,
			"-v", dir+":/etc/kubernetes/pki:ro", "--entrypoint", "/bin/sh", "fws/kubelet:1.35", "-c",
			"fws-kube-proxy --kubeconfig="+pkiDir+"kube-proxy.conf & "+
				"exec kubelet --kubeconfig="+pkiDir+node+".conf --container-runtime-endpoint=unix://$FWS_ENGINE --hostname-override="+node+
				" --cgroups-per-qos=false --enforce-node-allocatable= --fail-swap-on=false --eviction-hard='memory.available<10Mi' --resolv-conf=/dev/null"+
				" --register-with-taints=e2e/loading=true:NoSchedule")
		mustKubectl("wait", "--for=create", "--timeout=2m", "node/"+node)
		mustKubectl("wait", "--for=condition=Ready", "--timeout=2m", "node/"+node)
		if out := mustKubectl("get", "node/"+node, "-o", "jsonpath={.status.nodeInfo.operatingSystem} {.status.nodeInfo.kubeletVersion} {.status.nodeInfo.containerRuntimeVersion}"); !strings.HasPrefix(out, "darwin v1.35.0 fun-with-sandboxes://") {
			t.Errorf("%s's info: %q", node, out)
		}
		sock := strings.TrimSpace(mustDocker(t, "exec", container, "/bin/sh", "-c", "echo $FWS_ENGINE"))
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		save := exec.CommandContext(ctx, "docker", "save", "fws/pause:3.10.1", "fws/nginx:1", "fws/toolbox:1", "fws/kubectl:1.35")
		load := exec.CommandContext(ctx, "docker", "-H", "unix://"+sock, "load")
		pipe, err := save.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		load.Stdin = pipe
		if err := save.Start(); err != nil {
			t.Fatal(err)
		}
		if out, err := load.CombinedOutput(); err != nil || save.Wait() != nil {
			t.Fatalf("loading images into %s's engine: %v\n%s", node, err, out)
		}
		mustKubectl("taint", "nodes", node, "e2e/loading-")
		return sock
	}
	sock := joinNode("node1", "e2e-kubelet")
	nodeSock = sock
	mustKubectl("rollout", "status", "--timeout=3m", "deployment/web")
	pods := strings.Fields(mustKubectl("get", "pods", "-l", "app=web", "-o", "jsonpath={range .items[*]}{.metadata.name}={.spec.nodeName}={.status.podIP} {end}"))
	if len(pods) != 2 {
		t.Fatalf("the Deployment's pods: %q", pods)
	}
	// Each pod's address is on the pod network, in its node's pod CIDR.
	podCIDR := mustKubectl("get", "node/node1", "-o", "jsonpath={.spec.podCIDR}")
	cidr, err := netip.ParsePrefix(podCIDR)
	if err != nil || !netip.MustParsePrefix("10.244.0.0/16").Contains(cidr.Addr()) {
		t.Fatalf("node1's pod CIDR: %q, %v", podCIDR, err)
	}
	for _, p := range pods {
		f := strings.Split(p, "=")
		a, err := netip.ParseAddr(f[2])
		if f[1] != "node1" || err != nil || !cidr.Contains(a) {
			t.Errorf("pod %s ran on %q with address %q, want one in %s", f[0], f[1], f[2], cidr)
		}
	}
	// A pod reaches another at its address, as on a cluster's pod network.
	target := strings.Split(pods[1], "=")[2]
	mustKubectl("run", "fetcher", "--image=fws/toolbox:1", "--restart=Never", "--", "fetch", "http://"+target+"/")
	if _, err := kubectl("wait", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=2m", "pod/fetcher"); err != nil {
		out, _ := kubectl("logs", "fetcher")
		t.Errorf("a pod fetching http://%s/ from another: %v\n%s", target, err, out)
	} else if out := mustKubectl("logs", "fetcher"); out != "200 OK\n" {
		t.Errorf("a pod fetching http://%s/ from another: %q", target, out)
	}
	// kubectl exec reaches the runtime's stream server through the kubelet.
	web := strings.Split(pods[0], "=")[0]
	if out := mustKubectl("exec", web, "--", "/bin/sh", "-c", "echo inside $HOSTNAME"); out != "inside "+web+"\n" {
		t.Errorf("kubectl exec: %q", out)
	}
	// A Service: pods reach it by its DNS name and at its cluster IP, and
	// the engine carries each connection to one of its pods. A fetch
	// restarts until fws-kube-proxy has passed the Service on.
	mustKubectl("expose", "deployment/web", "--port=80", "--name=web")
	clusterIP := mustKubectl("get", "service/web", "-o", "jsonpath={.spec.clusterIP}")
	for name, url := range map[string]string{"by-name": "http://web.default.svc.cluster.local/", "by-ip": "http://" + clusterIP + "/"} {
		pod := "service-" + name
		mustKubectl("run", pod, "--image=fws/toolbox:1", "--restart=OnFailure", "--", "fetch", url)
		if _, err := kubectl("wait", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=2m", "pod/"+pod); err != nil {
			out, _ := kubectl("logs", pod)
			t.Errorf("a pod fetching the Service at %s: %v\n%s", url, err, out)
		} else if out := mustKubectl("logs", pod); out != "200 OK\n" {
			t.Errorf("a pod fetching the Service at %s: %q", url, out)
		}
	}
	// A NodePort Service answers on the node: a container on the
	// cluster's network reaches it at node1's port, as a client outside a
	// kind cluster reaches a node's.
	mustKubectl("expose", "deployment/web", "--type=NodePort", "--port=80", "--name=web-nodeport")
	nodePort := mustKubectl("get", "service/web-nodeport", "-o", "jsonpath={.spec.ports[0].nodePort}")
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(2 * time.Second) {
		out, err := docker(t, "run", "--rm", "--network", network, "fws/toolbox:1", "fetch", "http://node1:"+nodePort+"/")
		if err == nil && out == "200 OK\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("node1's node port %s: %q, %v", nodePort, out, err)
			break
		}
	}
	// The kubernetes Service reaches the API server, beyond the pod
	// network: an in-cluster client, kubectl with the pod's service
	// account, asks for its health.
	mustKubectl("run", "in-cluster", "--image=fws/kubectl:1.35", "--restart=OnFailure", "--command", "--", "kubectl", "get", "--raw", "/healthz")
	if _, err := kubectl("wait", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=2m", "pod/in-cluster"); err != nil {
		out, _ := kubectl("logs", "in-cluster")
		t.Errorf("kubectl in a pod, through the kubernetes Service: %v\n%s", err, out)
	} else if out := mustKubectl("logs", "in-cluster"); out != "ok" && out != "ok\n" {
		t.Errorf("kubectl in a pod, through the kubernetes Service: %q", out)
	}
	// The API server reads a pod's log from the kubelet, which reads the
	// file the runtime writes.
	mustKubectl("run", "greeter", "--image=fws/toolbox:1", "--restart=Never", "--", "echo", "from the node")
	mustKubectl("wait", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=2m", "pod/greeter")
	if out := mustKubectl("logs", "greeter"); out != "from the node\n" {
		t.Errorf("the greeter's log: %q", out)
	}
	// A second node: its pods reach node1's at their addresses, and the
	// Service, whose pods are node1's, by name, as on a cluster's pod
	// network across nodes.
	joinNode("node2", "e2e-kubelet2")
	if podCIDR2 := mustKubectl("get", "node/node2", "-o", "jsonpath={.spec.podCIDR}"); podCIDR2 == "" || podCIDR2 == podCIDR {
		t.Errorf("node2's pod CIDR %q, beside node1's %q", podCIDR2, podCIDR)
	}
	for name, url := range map[string]string{"pod": "http://" + target + "/", "service": "http://web.default.svc.cluster.local/"} {
		pod := "node2-to-" + name
		mustKubectl("run", pod, "--image=fws/toolbox:1", "--restart=OnFailure", `--overrides={"spec":{"nodeName":"node2"}}`, "--", "fetch", url)
		if _, err := kubectl("wait", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=2m", "pod/"+pod); err != nil {
			out, _ := kubectl("logs", pod)
			t.Errorf("a pod on node2 fetching %s: %v\n%s", url, err, out)
		} else if out := mustKubectl("logs", pod); out != "200 OK\n" {
			t.Errorf("a pod on node2 fetching %s: %q", url, out)
		}
	}
	mustKubectl("scale", "deployment/web", "--replicas=0")
	mustKubectl("wait", "--for=delete", "--timeout=2m", "pod", "-l", "app=web")
	if out := mustDocker(t, "-H", "unix://"+sock, "ps", "--format", "{{.Names}}"); strings.Contains(out, "k8s_nginx") {
		t.Errorf("the node's engine still runs the deleted pods' containers:\n%s", out)
	}
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
