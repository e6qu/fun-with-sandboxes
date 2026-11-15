package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuild(t *testing.T) {
	var svcs struct{ Items []k8sService }
	var slices struct{ Items []endpointSlice }
	if err := json.Unmarshal([]byte(`{"items":[
	  {"metadata":{"name":"web","namespace":"default"},"spec":{"type":"NodePort","clusterIP":"10.96.0.10","ports":[{"name":"http","port":80,"protocol":"TCP","nodePort":30080},{"name":"dns","port":53,"protocol":"UDP","nodePort":30053}],
	   "externalIPs":["192.0.2.10","2001:db8::10","10.96.0.20"],"sessionAffinity":"ClientIP","sessionAffinityConfig":{"clientIP":{"timeoutSeconds":600}}}},
	  {"metadata":{"name":"headless","namespace":"default"},"spec":{"clusterIP":"None","ports":[{"port":80}]}},
	  {"metadata":{"name":"ext","namespace":"default"},"spec":{"type":"ExternalName","externalName":"db.example.com","ports":[{"port":5432}]}},
	  {"metadata":{"name":"badext","namespace":"default"},"spec":{"type":"ExternalName","externalName":"not a name"}},
	  {"metadata":{"name":"lb","namespace":"default"},"spec":{"type":"LoadBalancer","clusterIP":"10.96.0.30","externalIPs":["192.0.2.10"],"ports":[{"port":80,"nodePort":30081}]},
	   "status":{"loadBalancer":{"ingress":[{"ip":"192.0.2.30"},{"hostname":"lb.example"}]}}},
	  {"metadata":{"name":"six","namespace":"default"},"spec":{"type":"ClusterIP","clusterIP":"fd00::7","clusterIPs":["fd00::7"],"sessionAffinity":"None","ports":[{"port":80}]}},
	  {"metadata":{"name":"api","namespace":"apps"},"spec":{"type":"ClusterIP","clusterIPs":["fd00::5","10.96.0.20"],"ports":[{"port":8443,"nodePort":31000}]}}
	]}`), &svcs); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"items":[
	  {"metadata":{"namespace":"default","labels":{"kubernetes.io/service-name":"web"}},"addressType":"IPv4",
	   "endpoints":[{"addresses":["10.244.0.6"],"conditions":{"ready":true}},{"addresses":["10.244.0.5"]},{"addresses":["10.244.0.7"],"conditions":{"ready":false}}],
	   "ports":[{"name":"http","port":8080,"protocol":"TCP"},{"name":"dns","port":5353,"protocol":"UDP"}]},
	  {"metadata":{"namespace":"default","labels":{"kubernetes.io/service-name":"web"}},"addressType":"IPv6",
	   "endpoints":[{"addresses":["fd00::6"]}],"ports":[{"name":"http","port":8080}]},
	  {"metadata":{"namespace":"default","labels":{"kubernetes.io/service-name":"six"}},"addressType":"IPv6",
	   "endpoints":[{"addresses":["fd00::8"]}],"ports":[{"port":8080}]},
	  {"metadata":{"namespace":"apps","labels":{"kubernetes.io/service-name":"api"}},"addressType":"IPv4",
	   "endpoints":[{"addresses":["10.244.0.9"]}],"ports":[{"port":9443}]}
	]}`), &slices); err != nil {
		t.Fatal(err)
	}
	got := build(svcs.Items, slices.Items)
	want := []Service{
		{Name: "api", Namespace: "apps", ClusterIP: "fd00::5", ClusterIPs: []string{"fd00::5", "10.96.0.20"},
			Ports: []ServicePort{{Port: 8443, Protocol: "TCP", Endpoints: []string{"10.244.0.9:9443"}}}},
		{Name: "ext", Namespace: "default", ExternalName: "db.example.com", Ports: []ServicePort{}},
		{Name: "lb", Namespace: "default", ClusterIP: "10.96.0.30", ExternalIPs: []string{"192.0.2.10", "192.0.2.30"},
			Ports: []ServicePort{{Port: 80, Protocol: "TCP", NodePort: 30081, Endpoints: []string{}}}},
		{Name: "six", Namespace: "default", ClusterIP: "fd00::7", Ports: []ServicePort{{Port: 80, Protocol: "TCP", Endpoints: []string{"[fd00::8]:8080"}}}},
		{Name: "web", Namespace: "default", ClusterIP: "10.96.0.10", SessionAffinity: "ClientIP", AffinitySeconds: 600,
			Ports: []ServicePort{{Port: 80, Protocol: "TCP", NodePort: 30080, Endpoints: []string{"10.244.0.5:8080", "10.244.0.6:8080"}},
				{Port: 53, Protocol: "UDP", NodePort: 30053, Endpoints: []string{"10.244.0.5:5353", "10.244.0.6:5353"}}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("build:\n%+v\nwant:\n%+v", got, want)
	}
}

func TestLoadKubeconfig(t *testing.T) {
	dir := t.TempDir()
	conf := "apiVersion: v1\nkind: Config\ncurrent-context: c\nclusters:\n- name: k\n  cluster:\n    server: https://api:6443/\ncontexts:\n- name: c\n  context:\n    cluster: k\n    user: u\nusers:\n- name: u\n  user:\n    tokenFile: token\n"
	os.WriteFile(filepath.Join(dir, "config"), []byte(conf), 0o600)
	os.WriteFile(filepath.Join(dir, "token"), []byte("secret\n"), 0o600)
	c, err := loadKubeconfig(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if c.server != "https://api:6443" || c.token != "secret" {
		t.Errorf("server %q, token %q", c.server, c.token)
	}
	os.WriteFile(filepath.Join(dir, "bad"), []byte("current-context: missing\n"), 0o600)
	if _, err := loadKubeconfig(filepath.Join(dir, "bad")); err == nil {
		t.Error("a kubeconfig without its current context loaded")
	}
}
