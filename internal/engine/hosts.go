package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeHostsFiles writes the container's /etc/hosts and /etc/hostname, as
// Docker does: localhost, the container's hostname, which a container
// reaches on loopback, and its --add-host entries, where host-gateway names
// the host, also loopback here. A mount at either path stands instead.
func writeHostsFiles(root, hostname string, extra []string, mounts []mountRec) error {
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
	fmt.Fprintf(&b, "127.0.0.1\t%s\n", hostname)
	files := map[string]string{"/etc/hosts": b.String(), "/etc/hostname": hostname + "\n"}
	for p, content := range files {
		if mounted[p] {
			continue
		}
		dst := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
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
