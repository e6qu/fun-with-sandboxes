package pathmap

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Network.framework, which resolves through the private dnssd client
// rather than getaddrinfo, reaches a member of the container's stack by
// name, at the member's address, over the connection it makes with
// connectx; and the host's names resolve as before.
func TestNetworkFrameworkReachesMembers(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "nwprobe")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-Werror", "-framework", "Network", "-o", probe, "testdata/nwprobe/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// db listens at its host port, as another member does.
	db, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	accepted := make(chan struct{}, 4)
	go func() {
		for {
			c, err := db.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			c.Close()
		}
	}()
	network := filepath.Join(dir, "network")
	members := fmt.Sprintf("127.88.0.2\tapp\t\t0\tproj_default\t0\n127.88.0.3\tdb proj-db-1\t80/tcp=%d\t0\tproj_default\t0\n", db.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(network, []byte(members), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(host, port string) string {
		cmd := exec.Command(probe, host, port)
		cmd.Env = []string{"DYLD_INSERT_LIBRARIES=" + lib, NetworkVar + "=" + network, AddressVar + "=127.88.0.2"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v\n%s", host, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for _, name := range []string{"db", "proj-db-1", "db."} {
		if out := run(name, "80"); out != "ready 127.88.0.3" {
			t.Errorf("%s: %q, want ready 127.88.0.3", name, out)
			continue
		}
		select {
		case <-accepted:
		case <-time.After(10 * time.Second):
			t.Errorf("%s: the member's listener took no connection", name)
		}
	}
	// A name neither the stack nor the host answers fails as it would
	// without the library, and loopback is the host's.
	if out := run("nothing-answers.invalid", "80"); strings.HasPrefix(out, "ready") {
		t.Errorf("nothing-answers.invalid: %q", out)
	}
	if out := run("localhost", "1"); !strings.HasPrefix(out, "waiting") && !strings.HasPrefix(out, "failed") {
		t.Errorf("localhost: %q, want the host's refusal", out)
	}
}
