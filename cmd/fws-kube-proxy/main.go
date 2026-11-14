// Command fws-kube-proxy is kube-proxy for a node whose runtime is the
// engine: it watches the cluster's Services and EndpointSlices and keeps
// the engine's services on the node's pod network to match, so a pod
// reaches a Service at its cluster IP and by its DNS names, and the engine
// carries each connection to one of the Service's ready endpoints.
//
// It polls the API server, which needs no client library, and sends the
// engine the whole list when it changes, and again now and then, so an
// engine that restarted gets it back.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig of a user that may list Services and EndpointSlices, such as system:kube-proxy")
	engineSock := flag.String("engine", os.Getenv("FWS_ENGINE"), "the engine's socket (default $FWS_ENGINE)")
	network := flag.String("network", "fws-pods", "the engine's network the Services are on")
	interval := flag.Duration("interval", time.Second, "how often to read the Services")
	resync := flag.Duration("resync", 30*time.Second, "how often to send the engine an unchanged list")
	flag.Parse()
	if *kubeconfig == "" || *engineSock == "" {
		flag.Usage()
		os.Exit(2)
	}
	api, err := loadKubeconfig(*kubeconfig)
	if err != nil {
		log.Fatal(err)
	}
	eng := engineClient(*engineSock)
	var last []byte
	var sent time.Time
	for ; ; time.Sleep(*interval) {
		svcs, err := api.services(context.Background())
		if err != nil {
			log.Print(err)
			continue
		}
		b, _ := json.Marshal(svcs)
		if bytes.Equal(b, last) && time.Since(sent) < *resync {
			continue
		}
		if err := eng.put(*network, b); err != nil {
			log.Print(err)
			continue
		}
		if !bytes.Equal(b, last) {
			log.Printf("%d services on %s", len(svcs), *network)
		}
		last, sent = b, time.Now()
	}
}

// Service and ServicePort are the engine's services, as its API takes them.
type Service struct {
	Name      string        `json:"name"`
	Namespace string        `json:"namespace"`
	ClusterIP string        `json:"clusterIP"`
	Ports     []ServicePort `json:"ports"`
}

type ServicePort struct {
	Port      int      `json:"port"`
	Protocol  string   `json:"protocol"`
	NodePort  int      `json:"nodePort,omitempty"`
	Endpoints []string `json:"endpoints"`
}

// The parts of Kubernetes' objects the proxy reads.
type k8sService struct {
	Metadata struct{ Name, Namespace string } `json:"metadata"`
	Spec     struct {
		Type       string   `json:"type"`
		ClusterIP  string   `json:"clusterIP"`
		ClusterIPs []string `json:"clusterIPs"`
		Ports      []struct {
			Name     string `json:"name"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
			NodePort int    `json:"nodePort"`
		} `json:"ports"`
	} `json:"spec"`
}

type endpointSlice struct {
	Metadata struct {
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	AddressType string `json:"addressType"`
	Endpoints   []struct {
		Addresses  []string `json:"addresses"`
		Conditions struct {
			Ready *bool `json:"ready"`
		} `json:"conditions"`
	} `json:"endpoints"`
	Ports []struct {
		Name     *string `json:"name"`
		Port     *int    `json:"port"`
		Protocol *string `json:"protocol"`
	} `json:"ports"`
}

// build turns the cluster's Services and EndpointSlices into the engine's
// services: each IPv4 cluster IP, with its TCP and UDP ports, the TCP
// ports' node ports where the Service has them, and, behind each, the ready IPv4 endpoints
// of the slices that name it, at the slice port of the same name.
// Headless Services have no cluster IP to serve.
func build(svcs []k8sService, slices []endpointSlice) []Service {
	type key struct{ ns, name string }
	byService := map[key][]endpointSlice{}
	for _, s := range slices {
		if s.AddressType != "IPv4" {
			continue
		}
		n := s.Metadata.Labels["kubernetes.io/service-name"]
		if n != "" {
			byService[key{s.Metadata.Namespace, n}] = append(byService[key{s.Metadata.Namespace, n}], s)
		}
	}
	var out []Service
	for _, k := range svcs {
		ip := k.Spec.ClusterIP
		if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() {
			ip = ""
			for _, c := range k.Spec.ClusterIPs {
				if a, err := netip.ParseAddr(c); err == nil && a.Is4() {
					ip = c
					break
				}
			}
		}
		if ip == "" {
			continue
		}
		svc := Service{Name: k.Metadata.Name, Namespace: k.Metadata.Namespace, ClusterIP: ip}
		for _, p := range k.Spec.Ports {
			proto := p.Protocol
			if proto == "" {
				proto = "TCP"
			}
			if proto != "TCP" && proto != "UDP" {
				continue
			}
			sp := ServicePort{Port: p.Port, Protocol: proto, Endpoints: []string{}}
			// A NodePort Service, and a LoadBalancer one, which is one
			// too, answers on the node's port as well, for TCP.
			if proto == "TCP" && (k.Spec.Type == "NodePort" || k.Spec.Type == "LoadBalancer") {
				sp.NodePort = p.NodePort
			}
			for _, s := range byService[key{svc.Namespace, svc.Name}] {
				target := 0
				for _, q := range s.Ports {
					name := ""
					if q.Name != nil {
						name = *q.Name
					}
					qp := "TCP"
					if q.Protocol != nil {
						qp = *q.Protocol
					}
					if name == p.Name && q.Port != nil && qp == proto {
						target = *q.Port
					}
				}
				if target == 0 {
					continue
				}
				for _, ep := range s.Endpoints {
					if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
						continue
					}
					for _, a := range ep.Addresses {
						if addr, err := netip.ParseAddr(a); err == nil && addr.Is4() {
							sp.Endpoints = append(sp.Endpoints, netip.AddrPortFrom(addr, uint16(target)).String())
						}
					}
				}
			}
			sort.Strings(sp.Endpoints)
			svc.Ports = append(svc.Ports, sp)
		}
		out = append(out, svc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// apiClient reads the API server as a kubeconfig's current context names
// it.
type apiClient struct {
	server string
	token  string
	http   *http.Client
}

func (c *apiClient) list(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.server+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("GET %s: %s: %s", path, res.Status, bytes.TrimSpace(b))
	}
	return json.NewDecoder(res.Body).Decode(into)
}

func (c *apiClient) services(ctx context.Context) ([]Service, error) {
	var svcs struct{ Items []k8sService }
	if err := c.list(ctx, "/api/v1/services", &svcs); err != nil {
		return nil, err
	}
	var slices struct{ Items []endpointSlice }
	if err := c.list(ctx, "/apis/discovery.k8s.io/v1/endpointslices", &slices); err != nil {
		return nil, err
	}
	return build(svcs.Items, slices.Items), nil
}

type kubeconfigFile struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			ClientCertificate     string `yaml:"client-certificate"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKey             string `yaml:"client-key"`
			ClientKeyData         string `yaml:"client-key-data"`
			Token                 string `yaml:"token"`
			TokenFile             string `yaml:"tokenFile"`
		} `yaml:"user"`
	} `yaml:"users"`
}

// loadKubeconfig reads the server, CA and credentials of a kubeconfig's
// current context: client certificates or a bearer token, inline or in
// files, which a relative path names beside the kubeconfig.
func loadKubeconfig(path string) (*apiClient, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kc kubeconfigFile
	if err := yaml.Unmarshal(b, &kc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	read := func(data, file string) ([]byte, error) {
		if data != "" {
			return base64.StdEncoding.DecodeString(data)
		}
		if file == "" {
			return nil, nil
		}
		if !filepath.IsAbs(file) {
			file = filepath.Join(dir, file)
		}
		return os.ReadFile(file)
	}
	var cluster, user string
	for _, c := range kc.Contexts {
		if c.Name == kc.CurrentContext {
			cluster, user = c.Context.Cluster, c.Context.User
		}
	}
	if cluster == "" {
		return nil, fmt.Errorf("%s: no context %q", path, kc.CurrentContext)
	}
	c := &apiClient{}
	tc := &tls.Config{}
	found := false
	for _, cl := range kc.Clusters {
		if cl.Name != cluster {
			continue
		}
		found = true
		if _, err := url.Parse(cl.Cluster.Server); err != nil || cl.Cluster.Server == "" {
			return nil, fmt.Errorf("%s: cluster %s has no server", path, cluster)
		}
		c.server = strings.TrimSuffix(cl.Cluster.Server, "/")
		ca, err := read(cl.Cluster.CertificateAuthorityData, cl.Cluster.CertificateAuthority)
		if err != nil {
			return nil, err
		}
		if len(ca) > 0 {
			tc.RootCAs = x509.NewCertPool()
			if !tc.RootCAs.AppendCertsFromPEM(ca) {
				return nil, fmt.Errorf("%s: cluster %s's CA holds no certificate", path, cluster)
			}
		}
		tc.InsecureSkipVerify = cl.Cluster.InsecureSkipTLSVerify
	}
	if !found {
		return nil, fmt.Errorf("%s: no cluster %q", path, cluster)
	}
	for _, u := range kc.Users {
		if u.Name != user {
			continue
		}
		crt, err := read(u.User.ClientCertificateData, u.User.ClientCertificate)
		if err != nil {
			return nil, err
		}
		key, err := read(u.User.ClientKeyData, u.User.ClientKey)
		if err != nil {
			return nil, err
		}
		if len(crt) > 0 {
			pair, err := tls.X509KeyPair(crt, key)
			if err != nil {
				return nil, fmt.Errorf("%s: user %s: %w", path, user, err)
			}
			tc.Certificates = []tls.Certificate{pair}
		}
		c.token = u.User.Token
		if c.token == "" && u.User.TokenFile != "" {
			t, err := read("", u.User.TokenFile)
			if err != nil {
				return nil, err
			}
			c.token = strings.TrimSpace(string(t))
		}
	}
	c.http = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tc, Proxy: http.ProxyFromEnvironment}}
	return c, nil
}

// engine is a client of the engine's API on its Unix socket.
type engine struct{ http *http.Client }

func engineClient(sock string) engine {
	sock = strings.TrimPrefix(sock, "unix://")
	return engine{&http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}}
}

func (e engine) put(network string, body []byte) error {
	req, err := http.NewRequest("PUT", "http://engine/fws/networks/"+url.PathEscape(network)+"/services", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("engine: %s: %s", res.Status, bytes.TrimSpace(b))
	}
	return nil
}
