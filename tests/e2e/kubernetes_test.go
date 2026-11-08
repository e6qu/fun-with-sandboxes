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
		{"kubelet", "system:node:node1", "system:nodes"},
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
	mustDocker(t, "network", "create", network)
	defer docker(t, "network", "rm", network)
	run := func(name string, args ...string) {
		t.Helper()
		mustDocker(t, append([]string{"run", "-d", "--restart=on-failure", "--name", "e2e-" + name, "--network", network, "--network-alias", name,
			"-v", dir + ":/etc/kubernetes/pki:ro"}, args...)...)
	}
	names := []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kubelet"}
	defer func() {
		if t.Failed() {
			for _, n := range names {
				out, _ := docker(t, "logs", "e2e-"+n)
				t.Logf("%s's log:\n%s", n, tail(out, 40))
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
		out, err := exec.CommandContext(ctx, "docker", append([]string{"run", "--rm", "--network", network,
			"-v", dir + ":/etc/kubernetes/pki:ro", "fws/kubectl:1.35", "kubectl", "--kubeconfig=" + pkiDir + "admin.conf"}, args...)...).CombinedOutput()
		return string(out), err
	}
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
		"--controllers=*,bootstrapsigner,tokencleaner")
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
	// the sandbox's pause image and the Deployment's.
	mustDocker(t, "run", "-d", "--privileged", "--restart=on-failure", "--name", "e2e-kubelet", "--network", network, "--network-alias", "node1",
		"-v", dir+":/etc/kubernetes/pki:ro", "--entrypoint", "/bin/sh", "fws/kubelet:1.35", "-c",
		"exec kubelet --kubeconfig="+pkiDir+"kubelet.conf --container-runtime-endpoint=unix://$FWS_ENGINE --hostname-override=node1"+
			" --cgroups-per-qos=false --enforce-node-allocatable= --fail-swap-on=false --eviction-hard=memory.available<10Mi"+
			" --register-with-taints=e2e/loading=true:NoSchedule")
	mustKubectl("wait", "--for=create", "--timeout=2m", "node/node1")
	mustKubectl("wait", "--for=condition=Ready", "--timeout=2m", "node/node1")
	if out := mustKubectl("get", "node/node1", "-o", "jsonpath={.status.nodeInfo.operatingSystem} {.status.nodeInfo.kubeletVersion} {.status.nodeInfo.containerRuntimeVersion}"); !strings.HasPrefix(out, "darwin v1.35.0 fun-with-sandboxes://") {
		t.Errorf("the node's info: %q", out)
	}
	sock := strings.TrimSpace(mustDocker(t, "exec", "e2e-kubelet", "/bin/sh", "-c", "echo $FWS_ENGINE"))
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	save := exec.CommandContext(ctx, "docker", "save", "fws/pause:3.10.1", "fws/nginx:1", "fws/toolbox:1")
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
		t.Fatalf("loading images into the node's engine: %v\n%s", err, out)
	}
	mustKubectl("taint", "nodes", "node1", "e2e/loading-")
	mustKubectl("rollout", "status", "--timeout=3m", "deployment/web")
	pods := strings.Fields(mustKubectl("get", "pods", "-l", "app=web", "-o", "jsonpath={range .items[*]}{.metadata.name}={.spec.nodeName}={.status.podIP} {end}"))
	if len(pods) != 2 {
		t.Fatalf("the Deployment's pods: %q", pods)
	}
	for _, p := range pods {
		f := strings.Split(p, "=")
		if f[1] != "node1" || f[2] == "" {
			t.Errorf("pod %s ran on %q with address %q", f[0], f[1], f[2])
		}
	}
	// The API server reads a pod's log from the kubelet, which reads the
	// file the runtime writes.
	mustKubectl("run", "greeter", "--image=fws/toolbox:1", "--restart=Never", "--", "echo", "from the node")
	mustKubectl("wait", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=2m", "pod/greeter")
	if out := mustKubectl("logs", "greeter"); out != "from the node\n" {
		t.Errorf("the greeter's log: %q", out)
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
