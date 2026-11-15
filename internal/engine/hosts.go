package engine

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"github.com/e6qu/fun-with-sandboxes/internal/rootfs"
)

// writeHosts writes c's /etc/hosts and /etc/hostname as it starts, as
// Docker writes them for each run: those of the container whose network
// it is on, with that container's legacy links.
func (e *Engine) writeHosts(c *Container) error {
	ne, net, err := e.netOf(c)
	if err != nil {
		return err
	}
	links, err := ne.linksOf(net)
	if err != nil {
		return err
	}
	var addresses []string
	if onNetwork(net) {
		addresses = firstAddresses(net.rec.Address, net.rec.Networks)
	}
	return writeHostsFiles(c.root(), hostnameOf(net), net.rec.Config.Domainname, addresses, net.rec.HostConfig.ExtraHosts, linkHosts(links), dnsOf(net.rec.HostConfig), c.rec.Mounts)
}

// dnsSettings are a container's DNS servers, search domains and options
// (--dns, --dns-search, --dns-option).
type dnsSettings struct {
	Servers         []netip.Addr
	Search, Options []string
}

func dnsOf(hc container.HostConfig) dnsSettings {
	return dnsSettings{Servers: hc.DNS, Search: hc.DNSSearch, Options: hc.DNSOptions}
}

// set reports whether the container has DNS settings of its own: its
// router then resolves the names its stack does not answer, as its
// resolv.conf says (router.Resolve).
func (d dnsSettings) set() bool {
	return len(d.Servers) > 0 || len(d.Search) > 0 || len(d.Options) > 0
}

// checkDNS refuses DNS settings the engine cannot honour or a resolv.conf
// cannot hold, with the reason.
func checkDNS(d dnsSettings) error {
	for _, a := range d.Servers {
		if a.Unmap().IsLoopback() || a.IsUnspecified() {
			return unsupported("DNS server %s is not supported: the container's router, outside its sandbox, asks its DNS servers, and a server on loopback, the container's own or a member's of its stack, is out of its reach", a)
		}
		if a.IsMulticast() || a.Zone() != "" {
			return invalid("invalid DNS server %s", a)
		}
	}
	for _, s := range d.Search {
		if s == "" || strings.ContainsAny(s, " \t\r\n#;") || len(s) > 253 {
			return invalid("invalid DNS search domain %q", s)
		}
	}
	for _, o := range d.Options {
		if o == "" || strings.ContainsAny(o, " \t\r\n#;") {
			return invalid("invalid DNS option %q", o)
		}
	}
	return nil
}

// containerResolvConf is the resolv.conf the engine writes for a
// container with settings d, from the host's.
func containerResolvConf(d dnsSettings) (string, error) {
	host, err := os.ReadFile("/etc/resolv.conf")
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return resolvConf(host, d), nil
}

// firstAddresses are the addresses a container at member address holds
// on the first of its networks, by name: its own there on a network with
// a subnet, none of IPv4 on an IPv6-only network, else its member
// address, and its IPv6 address there on a network with IPv6. Its
// hostname names them.
func firstAddresses(address string, nets map[string]*network.EndpointSettings) []string {
	names := make([]string, 0, len(nets))
	for n := range nets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		es := nets[n]
		if n == "none" || es == nil {
			continue
		}
		var out []string
		switch {
		case es.IPAddress.IsValid():
			out = append(out, es.IPAddress.String())
		case !es.GlobalIPv6Address.IsValid():
			out = append(out, address)
		}
		if es.GlobalIPv6Address.IsValid() {
			out = append(out, es.GlobalIPv6Address.String())
		}
		return out
	}
	return []string{address}
}

// writeHostsFiles writes the container's /etc/hosts and /etc/hostname, as
// Docker does: localhost, its --add-host entries, where host-gateway names
// the host, on loopback here, the container's hostname at its addresses,
// after the name its domain name makes it, or on loopback for a container
// on no network, and its links. A mount at either path stands instead.
func writeHostsFiles(root, hostname, domain string, addresses, extra, links []string, dns dnsSettings, mounts []mountRec) error {
	mounted := map[string]bool{}
	for _, m := range mounts {
		mounted[filepath.Clean(m.Destination)] = true
	}
	var b strings.Builder
	b.WriteString("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	for _, h := range extra {
		name, ip, ok := strings.Cut(h, ":")
		if !ok || name == "" || ip == "" {
			return invalid("invalid --add-host %q: want host:ip", h)
		}
		if ip == "host-gateway" {
			ip = "127.0.0.1"
		}
		fmt.Fprintf(&b, "%s\t%s\n", ip, name)
	}
	if len(addresses) == 0 || addresses[0] == "" {
		addresses = []string{"127.0.0.1"}
	}
	names := hostname
	if domain != "" {
		names = hostname + "." + domain + " " + hostname
	}
	for _, a := range addresses {
		fmt.Fprintf(&b, "%s\t%s\n", a, names)
	}
	for _, l := range links {
		b.WriteString(l + "\n")
	}
	resolv, err := containerResolvConf(dns)
	if err != nil {
		return err
	}
	files := map[string]string{"/etc/hosts": b.String(), "/etc/hostname": hostname + "\n", "/etc/resolv.conf": resolv}
	for p, content := range files {
		if mounted[p] {
			continue
		}
		dir, err := rootfs.Join(root, path.Dir(p))
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, path.Base(p))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		// The image's file may be a symlink, which must not be followed out
		// of the root.
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// resolvConf is a container's /etc/resolv.conf, as Docker writes it for a
// container on its default bridge: the host's, without the nameservers on
// loopback, which would be the container's own, and Google's public ones
// when none remain. The container's own servers, search domains and
// options replace the host's, and a search domain of "." alone leaves it
// none.
func resolvConf(host []byte, d dnsSettings) string {
	var b strings.Builder
	servers := 0
	for _, line := range strings.Split(string(host), "\n") {
		f := strings.Fields(line)
		if len(f) >= 1 {
			switch f[0] {
			case "nameserver":
				if len(d.Servers) > 0 {
					continue
				}
				if len(f) >= 2 {
					if ip := net.ParseIP(f[1]); ip != nil && ip.IsLoopback() {
						continue
					}
					servers++
				}
			case "search", "domain":
				if len(d.Search) > 0 {
					continue
				}
			case "options":
				if len(d.Options) > 0 {
					continue
				}
			}
		}
		if strings.TrimSpace(line) != "" {
			b.WriteString(line + "\n")
		}
	}
	for _, s := range d.Servers {
		fmt.Fprintf(&b, "nameserver %s\n", s)
		servers++
	}
	if servers == 0 {
		b.WriteString("nameserver 8.8.8.8\nnameserver 8.8.4.4\n")
	}
	if len(d.Search) > 0 && (len(d.Search) > 1 || d.Search[0] != ".") {
		b.WriteString("search " + strings.Join(d.Search, " ") + "\n")
	}
	if len(d.Options) > 0 {
		b.WriteString("options " + strings.Join(d.Options, " ") + "\n")
	}
	return b.String()
}
