package main

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"
)

// As k3s, the entry is fws/k3s's: k3s server brings up a single-node
// cluster as k3s does, with kubeadm on the parts fws/kind-node runs, and
// writes k3s's kubeconfig; k3s kubectl, ctr and crictl run those against
// it. Testcontainers' k3s module runs the image as it runs rancher/k3s.

const (
	adminKubeconfig = "/etc/kubernetes/admin.conf"
	// k3sDir holds k3s's kubeconfig, and the kubeadm configuration the
	// server writes.
	k3sDir          = "/etc/rancher/k3s"
	k3sKubeconfig   = k3sDir + "/k3s.yaml"
	k3sKubeadmFile  = k3sDir + "/kubeadm.yaml"
	k3sVersionFile  = "/kind/version"
	k3sReadyMessage = "Node controller sync successful"
)

func k3s(args []string) {
	if len(args) == 0 {
		log.Fatal("k3s: name a command: server, kubectl, ctr or crictl")
	}
	switch args[0] {
	case "server":
		k3sServer(args[1:])
	case "kubectl":
		if os.Getenv("KUBECONFIG") == "" {
			os.Setenv("KUBECONFIG", k3sKubeconfig)
		}
		if err := syscall.Exec("/usr/local/bin/kubectl", append([]string{"kubectl"}, args[1:]...), os.Environ()); err != nil {
			log.Fatalf("k3s kubectl: %v", err)
		}
	case "ctr":
		os.Exit(ctr(engineClient(os.Getenv("FWS_ENGINE")), args[1:], os.Stdin, os.Stdout))
	case "crictl":
		crictl(args[1:])
	case "--version", "-v", "version":
		v, _ := os.ReadFile(k3sVersionFile)
		fmt.Printf("k3s version %s+fws\n", strings.TrimSpace(string(v)))
	default:
		log.Fatalf("k3s %s: fws/k3s runs a server and its kubectl, ctr and crictl; it has no agent of its own", args[0])
	}
	os.Exit(0)
}

// k3sOptions are the k3s server flags fws/k3s takes.
type k3sOptions struct {
	NodeName       string
	TLSSANs        []string
	ClusterCIDR    string
	ServiceCIDR    string
	Kubeconfig     string
	KubeconfigMode os.FileMode
}

// k3sValueFlags are the k3s server flags that take a value, which may
// follow as the next argument; any other is a boolean.
var k3sValueFlags = map[string]bool{
	"node-name": true, "tls-san": true, "cluster-cidr": true, "service-cidr": true, "write-kubeconfig": true, "o": true,
	"write-kubeconfig-mode": true, "https-listen-port": true, "disable": true, "token": true, "t": true, "data-dir": true, "d": true,
	"node-ip": true, "node-external-ip": true, "node-label": true, "node-taint": true, "bind-address": true, "advertise-address": true,
	"cluster-dns": true, "cluster-domain": true, "kube-apiserver-arg": true, "kube-controller-manager-arg": true,
	"kube-scheduler-arg": true, "kubelet-arg": true, "kube-proxy-arg": true, "flannel-backend": true, "snapshotter": true,
}

// parseK3sServer reads k3s server's flags, and the environment variables
// k3s reads for them, as k3s takes them: --flag=value or --flag value.
// Flags that shape what fws/k3s does not run, as --disable=traefik,
// are taken and have no effect.
func parseK3sServer(args []string, getenv func(string) string) (k3sOptions, error) {
	o := k3sOptions{
		NodeName: getenv("K3S_NODE_NAME"), ClusterCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16",
		Kubeconfig: k3sKubeconfig, KubeconfigMode: 0o600,
	}
	mode := getenv("K3S_KUBECONFIG_MODE")
	if v := getenv("K3S_KUBECONFIG_OUTPUT"); v != "" {
		o.Kubeconfig = v
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return o, fmt.Errorf("k3s server takes no argument %q", a)
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !hasValue && k3sValueFlags[name] && i+1 < len(args) {
			i++
			value = args[i]
		}
		switch name {
		case "node-name":
			o.NodeName = value
		case "tls-san":
			for _, s := range strings.Split(value, ",") {
				if s != "" {
					o.TLSSANs = append(o.TLSSANs, s)
				}
			}
		case "cluster-cidr":
			o.ClusterCIDR = value
		case "service-cidr":
			o.ServiceCIDR = value
		case "write-kubeconfig", "o":
			o.Kubeconfig = value
		case "write-kubeconfig-mode":
			mode = value
		case "https-listen-port":
			if value != "6443" {
				return o, fmt.Errorf("k3s server: the API server listens on 6443 alone, not %s", value)
			}
		default:
			log.Printf("k3s server: --%s has no effect on fws/k3s", name)
		}
	}
	if mode != "" {
		m, err := strconv.ParseUint(mode, 8, 32)
		if err != nil {
			return o, fmt.Errorf("k3s server: kubeconfig mode %q: %v", mode, err)
		}
		o.KubeconfigMode = os.FileMode(m)
	}
	return o, nil
}

// kubeadmK3s is the kubeadm configuration of a k3s server: a control
// plane on the node's address that runs pods, as k3s's server node does,
// on k3s's pod and service subnets.
var kubeadmK3s = template.Must(template.New("kubeadm").Parse(`apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: "{{.Address}}"
  bindPort: 6443
nodeRegistration:
  name: "{{.Name}}"
  criSocket: unix:///run/containerd/containerd.sock
  taints: []
  kubeletExtraArgs:
  - name: node-ip
    value: "{{.Address}}"
  - name: provider-id
    value: "k3s://{{.Name}}"
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: "{{.Version}}"
clusterName: default
controlPlaneEndpoint: "{{.Address}}:6443"
apiServer:
  certSANs:
{{- range .SANs}}
  - "{{.}}"
{{- end}}
controllerManager:
  extraArgs:
  - name: enable-hostpath-provisioner
    value: "true"
networking:
  podSubnet: "{{.ClusterCIDR}}"
  serviceSubnet: "{{.ServiceCIDR}}"
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
failSwapOn: false
imageGCHighThresholdPercent: 100
evictionHard:
  nodefs.available: "0%"
  nodefs.inodesFree: "0%"
  imagefs.available: "0%"
`))

// k3sKubeadmConfig is the kubeadm configuration of a server on the node
// name at address.
func k3sKubeadmConfig(o k3sOptions, name, address, version string) ([]byte, error) {
	sans := append([]string{"localhost", "127.0.0.1", name}, o.TLSSANs...)
	var b bytes.Buffer
	err := kubeadmK3s.Execute(&b, map[string]any{
		"Name": name, "Address": address, "Version": version, "SANs": uniq(sans),
		"ClusterCIDR": o.ClusterCIDR, "ServiceCIDR": o.ServiceCIDR,
	})
	return b.Bytes(), err
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

var serverLine = regexp.MustCompile(`(?m)^(\s*server:\s*).*$`)

// k3sKubeconfigFrom is k3s's kubeconfig, from kubeadm's admin one: the
// API server at the node's loopback, as k3s writes it.
func k3sKubeconfigFrom(admin []byte) []byte {
	return serverLine.ReplaceAll(admin, []byte("${1}https://127.0.0.1:6443"))
}

// nodeAddress is the address the node's name resolves to, as k3s takes
// the address of its default route's interface: its first IPv4 address
// outside loopback, else its first.
func nodeAddress(name string) (string, error) {
	ips, err := net.LookupIP(name)
	if err != nil {
		return "", err
	}
	var first string
	for _, ip := range ips {
		if ip.To4() == nil {
			continue
		}
		if !ip.IsLoopback() {
			return ip.String(), nil
		}
		if first == "" {
			first = ip.String()
		}
	}
	if first == "" {
		return "", fmt.Errorf("%s has no IPv4 address", name)
	}
	return first, nil
}

// k3sServer brings up the node's cluster, as k3s server does, once: a
// node that restarts finds kubeadm's configuration and runs its kubelet
// on it again.
func k3sServer(args []string) {
	o, err := parseK3sServer(args, os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	n := boot("fws-k3s")
	defer n.stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); n.run() }()
	name := o.NodeName
	if name == "" {
		if name, err = os.Hostname(); err != nil {
			log.Fatal(err)
		}
	}
	name = strings.ToLower(name)
	if !exists(adminKubeconfig) {
		address, err := nodeAddress(name)
		if err != nil {
			log.Fatal(err)
		}
		if ip := net.ParseIP(address); ip != nil && ip.IsLoopback() {
			log.Printf("the node's address %s is a loopback one, which the kubelet refuses: run fws/k3s on a network with a subnet, as the default bridge network is", address)
		}
		v, err := os.ReadFile(k3sVersionFile)
		if err != nil {
			log.Fatal(err)
		}
		cfg, err := k3sKubeadmConfig(o, name, address, strings.TrimSpace(string(v)))
		if err != nil {
			log.Fatal(err)
		}
		if err := os.MkdirAll(k3sDir, 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(k3sKubeadmFile, cfg, 0o644); err != nil {
			log.Fatal(err)
		}
		init := exec.Command(realKubeadm, kubeadmArgs([]string{"init", "--config=" + k3sKubeadmFile, "--skip-token-print"})...)
		init.Stdout, init.Stderr = n.stdout, n.stdout
		if err := init.Run(); err != nil {
			log.Fatalf("kubeadm init: %v", err)
		}
	}
	for _, m := range []string{"/kind/manifests/default-cni.yaml", "/kind/manifests/default-storage.yaml"} {
		if err := retry(n, time.Minute, "kubectl", "--kubeconfig="+adminKubeconfig, "apply", "-f", m); err != nil {
			log.Fatalf("applying %s: %v", m, err)
		}
	}
	admin, err := os.ReadFile(adminKubeconfig)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(o.Kubeconfig), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(o.Kubeconfig, k3sKubeconfigFrom(admin), o.KubeconfigMode); err != nil {
		log.Fatal(err)
	}
	os.Chmod(o.Kubeconfig, o.KubeconfigMode)
	if err := retry(n, 5*time.Minute, "kubectl", "--kubeconfig="+adminKubeconfig, "wait", "--for=condition=Ready", "--timeout=30s", "node/"+name); err != nil {
		log.Fatalf("the node never became ready: %v", err)
	}
	// What Testcontainers' k3s module waits for.
	fmt.Fprintln(n.stdout, k3sReadyMessage)
	wg.Wait()
}

// retry runs a command until it succeeds or for d, as the node's control
// plane comes up.
func retry(n *node, d time.Duration, name string, args ...string) error {
	deadline := time.Now().Add(d)
	for {
		cmd := exec.CommandContext(n.ctx, name, args...)
		cmd.Stdout, cmd.Stderr = n.stdout, n.stdout
		err := cmd.Run()
		if err == nil || time.Now().After(deadline) || n.ctx.Err() != nil {
			return err
		}
		sleep(n.ctx, 2*time.Second)
	}
}
