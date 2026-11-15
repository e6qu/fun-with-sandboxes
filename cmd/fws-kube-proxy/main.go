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
	"slices"
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
	Name            string        `json:"name"`
	Namespace       string        `json:"namespace"`
	ClusterIP       string        `json:"clusterIP"`
	ClusterIPs      []string      `json:"clusterIPs,omitempty"`
	ExternalIPs     []string      `json:"externalIPs,omitempty"`
	SessionAffinity string        `json:"sessionAffinity,omitempty"`
	AffinitySeconds int           `json:"sessionAffinityTimeout,omitempty"`
	ExternalName    string        `json:"externalName,omitempty"`
	Ports           []ServicePort `json:"ports"`
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
		Type                  string   `json:"type"`
		ClusterIP             string   `json:"clusterIP"`
		ClusterIPs            []string `json:"clusterIPs"`
		ExternalIPs           []string `json:"externalIPs"`
		ExternalName          string   `json:"externalName"`
		SessionAffinity       string   `json:"sessionAffinity"`
		SessionAffinityConfig struct {
			ClientIP struct {
				TimeoutSeconds int `json:"timeoutSeconds"`
			} `json:"clientIP"`
		} `json:"sessionAffinityConfig"`
		Ports []struct {
			Name     string `json:"name"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
			NodePort int    `json:"nodePort"`
		} `json:"ports"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []struct {
				IP string `json:"ip"`
			} `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
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
// services: each cluster IP, IPv4 and IPv6, with its TCP and UDP ports,
// their node ports where the Service has them, its external IPs and load
// balancer addresses, its ClientIP session affinity, and, behind each
// port, the ready endpoints of the slices that name it, at the slice port
// of the same name: the IPv4 slices', or the IPv6 slices' for a Service
// without an IPv4 cluster IP.
// An ExternalName Service's names stand for its external name. Headless
// Services have no cluster IP to serve. An external or load
// balancer address another Service took first, or one of a family no
// cluster IP has, is left out, as the engine would refuse the whole list
// for it.
func build(svcs []k8sService, slices []endpointSlice) []Service {
	type key struct{ ns, name, family string }
	byService := map[key][]endpointSlice{}
	for _, s := range slices {
		if s.AddressType != "IPv4" && s.AddressType != "IPv6" {
			continue
		}
		n := s.Metadata.Labels["kubernetes.io/service-name"]
		if n != "" {
			k := key{s.Metadata.Namespace, n, s.AddressType}
			byService[k] = append(byService[k], s)
		}
	}
	sort.SliceStable(svcs, func(i, j int) bool {
		if svcs[i].Metadata.Namespace != svcs[j].Metadata.Namespace {
			return svcs[i].Metadata.Namespace < svcs[j].Metadata.Namespace
		}
		return svcs[i].Metadata.Name < svcs[j].Metadata.Name
	})
	taken := map[netip.Addr]bool{}
	for _, k := range svcs {
		for _, a := range clusterIPs(k) {
			taken[a] = true
		}
	}
	var out []Service
	for _, k := range svcs {
		if k.Spec.Type == "ExternalName" {
			if dnsName(k.Spec.ExternalName) {
				out = append(out, Service{Name: k.Metadata.Name, Namespace: k.Metadata.Namespace, ExternalName: k.Spec.ExternalName, Ports: []ServicePort{}})
			}
			continue
		}
		ips := clusterIPs(k)
		if len(ips) == 0 {
			continue
		}
		svc := Service{Name: k.Metadata.Name, Namespace: k.Metadata.Namespace, ClusterIP: ips[0].String()}
		family := "IPv6"
		has := map[bool]bool{}
		for _, a := range ips {
			has[a.Is4()] = true
			if a.Is4() {
				family = "IPv4"
			}
		}
		if len(ips) > 1 {
			for _, a := range ips {
				svc.ClusterIPs = append(svc.ClusterIPs, a.String())
			}
		}
		// A load balancer's addresses answer in the cluster as external
		// IPs do.
		external := append([]string(nil), k.Spec.ExternalIPs...)
		for _, in := range k.Status.LoadBalancer.Ingress {
			external = append(external, in.IP)
		}
		for _, x := range external {
			a, err := netip.ParseAddr(x)
			if err != nil || !usable(a) || !has[a.Is4()] || taken[a] {
				continue
			}
			taken[a] = true
			svc.ExternalIPs = append(svc.ExternalIPs, a.String())
		}
		if k.Spec.SessionAffinity == "ClientIP" {
			svc.SessionAffinity = "ClientIP"
			if t := k.Spec.SessionAffinityConfig.ClientIP.TimeoutSeconds; t > 0 && t <= 86400 {
				svc.AffinitySeconds = t
			}
		}
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
			// too, answers on the node's port as well.
			if k.Spec.Type == "NodePort" || k.Spec.Type == "LoadBalancer" {
				sp.NodePort = p.NodePort
			}
			for _, s := range byService[key{svc.Namespace, svc.Name, family}] {
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
						if addr, err := netip.ParseAddr(a); err == nil && addr.Is4() == (family == "IPv4") {
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
	return out
}

// clusterIPs are a Service's cluster IPs the engine serves, its primary
// first: one of each family at most.
func clusterIPs(k k8sService) []netip.Addr {
	var out []netip.Addr
	for _, c := range append([]string{k.Spec.ClusterIP}, k.Spec.ClusterIPs...) {
		a, err := netip.ParseAddr(c)
		if err != nil || !usable(a) || slices.ContainsFunc(out, func(o netip.Addr) bool { return o.Is4() == a.Is4() }) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// dnsName reports whether name is a DNS name the engine takes:
// dot-separated labels of letters, digits, hyphens and underscores, with a
// final dot or without.
func dnsName(name string) bool {
	name = strings.TrimSuffix(name, ".")
	if name == "" || len(name) > 253 {
		return false
	}
	for _, l := range strings.Split(name, ".") {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return false
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// usable reports whether the engine serves a service at address a: a
// unicast IPv4 or IPv6 address outside loopback.
func usable(a netip.Addr) bool {
	return a.Zone() == "" && !a.Is4In6() && !a.IsUnspecified() && !a.IsLoopback() && !a.IsMulticast()
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
