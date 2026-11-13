package main

import (
	"reflect"
	"strings"
	"testing"
)

// The flags Testcontainers' k3s module runs the server with.
func TestParseK3sServer(t *testing.T) {
	env := map[string]string{"K3S_KUBECONFIG_MODE": "644"}
	o, err := parseK3sServer([]string{"--disable=traefik", "--tls-san=localhost", "--tls-san", "a.example,b.example", "--node-name", "n1", "--cluster-init"},
		func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if o.NodeName != "n1" || !reflect.DeepEqual(o.TLSSANs, []string{"localhost", "a.example", "b.example"}) || o.KubeconfigMode != 0o644 ||
		o.Kubeconfig != k3sKubeconfig || o.ClusterCIDR != "10.42.0.0/16" || o.ServiceCIDR != "10.43.0.0/16" {
		t.Errorf("options %+v", o)
	}
	if _, err := parseK3sServer([]string{"--https-listen-port=7443"}, func(string) string { return "" }); err == nil {
		t.Error("another API server port was taken")
	}
	if _, err := parseK3sServer([]string{"agent"}, func(string) string { return "" }); err == nil {
		t.Error("an argument was taken")
	}
}

func TestK3sKubeadmConfig(t *testing.T) {
	o, _ := parseK3sServer([]string{"--tls-san=localhost", "--tls-san=docker.example"}, func(string) string { return "" })
	b, err := k3sKubeadmConfig(o, "n1", "172.17.0.2", "v1.35.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`advertiseAddress: "172.17.0.2"`, `name: "n1"`, "taints: []", `value: "172.17.0.2"`, `kubernetesVersion: "v1.35.0"`,
		`controlPlaneEndpoint: "172.17.0.2:6443"`, "  certSANs:\n  - \"localhost\"\n  - \"127.0.0.1\"\n  - \"n1\"\n  - \"docker.example\"\n",
		`podSubnet: "10.42.0.0/16"`, `serviceSubnet: "10.43.0.0/16"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the configuration has no %q:\n%s", want, b)
		}
	}
}

func TestK3sKubeconfigFrom(t *testing.T) {
	admin := "clusters:\n- cluster:\n    certificate-authority-data: X\n    server: https://172.17.0.2:6443\n  name: default\n"
	want := "clusters:\n- cluster:\n    certificate-authority-data: X\n    server: https://127.0.0.1:6443\n  name: default\n"
	if got := string(k3sKubeconfigFrom([]byte(admin))); got != want {
		t.Errorf("k3sKubeconfigFrom = %q", got)
	}
}
