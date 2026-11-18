package pathmap

import (
	"encoding/json"
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
// name, over the connection it makes with connectx, at the member's
// listener; and the host's names resolve as before.
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
		start := time.Now()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v\n%s%s", host, err, out, crashReport(t, "nwprobe", start))
		}
		return strings.TrimSpace(string(out))
	}
	for _, name := range []string{"db", "proj-db-1", "db."} {
		// The peer Network.framework reports is the kernel's, the host
		// port the library connected to, rather than the member's address.
		out := run(name, "80")
		if !strings.HasPrefix(out, "ready ") {
			t.Errorf("%s: %q, want a connection", name, out)
			continue
		}
		t.Logf("%s: %s", name, out)
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

// crashReport is the start of the newest report macOS wrote of a crash of
// program since start, which says where it crashed, or "".
func crashReport(t *testing.T, program string, start time.Time) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, "Library/Logs/DiagnosticReports")
	var newest string
	var when time.Time
	for i := 0; i < 20 && newest == ""; i++ {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			info, err := e.Info()
			if err == nil && strings.HasPrefix(e.Name(), program) && info.ModTime().After(start) && info.ModTime().After(when) {
				newest, when = filepath.Join(dir, e.Name()), info.ModTime()
			}
		}
		if newest == "" {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if newest == "" {
		return ""
	}
	b, err := os.ReadFile(newest)
	if err != nil {
		return ""
	}
	// A report is a line of JSON, its header, and then a document of
	// JSON that holds each thread's frames.
	_, body, _ := strings.Cut(string(b), "\n")
	var r struct {
		FaultingThread int `json:"faultingThread"`
		Exception      any `json:"exception"`
		Threads        []struct {
			Frames []struct {
				ImageIndex  int    `json:"imageIndex"`
				ImageOffset int    `json:"imageOffset"`
				Symbol      string `json:"symbol"`
			} `json:"frames"`
		} `json:"threads"`
		UsedImages []struct {
			Name string `json:"name"`
		} `json:"usedImages"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil || r.FaultingThread >= len(r.Threads) {
		return fmt.Sprintf("\n%s (%v):\n%.4000s", newest, err, b)
	}
	var lines []string
	for i, f := range r.Threads[r.FaultingThread].Frames {
		if i == 30 {
			break
		}
		image := "?"
		if f.ImageIndex < len(r.UsedImages) {
			image = r.UsedImages[f.ImageIndex].Name
		}
		lines = append(lines, fmt.Sprintf("  %2d %s %s +%#x", i, image, f.Symbol, f.ImageOffset))
	}
	return fmt.Sprintf("\n%s: exception %v, thread %d:\n%s", newest, r.Exception, r.FaultingThread, strings.Join(lines, "\n"))
}
