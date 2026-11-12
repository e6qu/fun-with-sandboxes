package main

import (
	"reflect"
	"testing"
)

func TestKubeadmArgs(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{[]string{"init", "--config=/kind/kubeadm.conf", "--skip-token-print", "--v=6"},
			[]string{"init", "--config=/kind/kubeadm.conf", "--skip-token-print", "--v=6", "--skip-phases=" + skipInit}},
		{[]string{"init", "--skip-phases=addon/x", "--v=6"}, []string{"init", "--v=6", "--skip-phases=" + skipInit + ",addon/x"}},
		{[]string{"init", "--skip-phases", "addon/x"}, []string{"init", "--skip-phases=" + skipInit + ",addon/x"}},
		{[]string{"join", "--config", "/kind/kubeadm.conf"}, []string{"join", "--config", "/kind/kubeadm.conf"}},
		{nil, nil},
	} {
		if got := kubeadmArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("kubeadmArgs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseFlags(t *testing.T) {
	env := "KUBELET_KUBEADM_ARGS=\"--container-runtime-endpoint=unix:///run/containerd/containerd.sock --node-ip=172.18.0.2 --node-labels= --provider-id=kind://docker/kind/kind-control-plane\"\n"
	want := []string{"--container-runtime-endpoint=unix:///run/containerd/containerd.sock", "--node-ip=172.18.0.2", "--node-labels=", "--provider-id=kind://docker/kind/kind-control-plane"}
	if got := parseFlags([]byte(env)); !reflect.DeepEqual(got, want) {
		t.Errorf("parseFlags = %q, want %q", got, want)
	}
}
