package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorageQuota(t *testing.T) {
	for opts, want := range map[string]int64{"": 0, "1G": 1 << 30, "100m": 100 << 20} {
		m := map[string]string{}
		if opts != "" {
			m["size"] = opts
		}
		got, err := storageQuota(m)
		if err != nil || got != want {
			t.Errorf("size %q: %d, %v; want %d", opts, got, err, want)
		}
	}
	for opts, kind := range map[[2]string]Kind{
		{"size", "lots"}: KindInvalid,
		{"size", "1m"}:   KindInvalid,
		{"inodes", "10"}: KindUnsupported,
	} {
		if _, err := storageQuota(map[string]string{opts[0]: opts[1]}); err == nil || KindOf(err) != kind {
			t.Errorf("%s=%s: %v, want kind %d", opts[0], opts[1], err, kind)
		}
	}
}

func TestTreeBytesCountsBlocks(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "b"), make([]byte, 5000), 0o644)
	os.Mkdir(filepath.Join(dir, "d"), 0o755)
	// The root and d a block each, a one block, b two.
	if n, err := treeBytes(dir); err != nil || n != 5*apfsBlock {
		t.Errorf("treeBytes = %d, %v; want %d", n, err, 5*apfsBlock)
	}
}

func TestCheckNetworkOptions(t *testing.T) {
	ok := map[string]string{
		"com.docker.network.bridge.enable_ip_masquerade": "true", "com.docker.network.bridge.enable_icc": "false",
		"com.docker.network.bridge.host_binding_ipv4": "127.0.0.1", "com.docker.network.bridge.name": "br-x",
		"com.docker.network.driver.mtu": "1500", "fws.egress.allow": "example.com",
	}
	if err := checkNetworkOptions(ok); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"com.docker.network.bridge.enable_icc":        "no",
		"com.docker.network.bridge.host_binding_ipv4": "::1",
		"com.docker.network.driver.mtu":               "20",
		"com.docker.network.bridge.gateway_mode_ipv4": "routed",
	} {
		if err := checkNetworkOptions(map[string]string{k: v}); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}
