package pathmap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// probe runs the probe under the library, with the container's root and
// identity in its environment, and returns what each check reported.
func probe(t *testing.T, root string, env []string, checks ...string) map[string]string {
	t.Helper()
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, "bin", "probe"), checks...)
	cmd.Env = append([]string{"DYLD_INSERT_LIBRARIES=" + lib, "SANDBOX_ROOT=" + root}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	return got
}

func containerRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"bin", "etc", "made"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "etc", "marker"), []byte("inside"), 0o644)
	os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\napp:x:1000:50:App:/home/app:/bin/sh\n"), 0o644)
	if out, err := exec.Command("go", "build", "-o", filepath.Join(root, "bin", "probe"), "./testdata/probe").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	return root
}

func TestPathsNameTheRoot(t *testing.T) {
	root := containerRoot(t)
	got := probe(t, root, nil, "read", "system", "write", "list", "cwd", "exec")
	for k, want := range map[string]string{"read": "inside", "system": "ok", "write": "ok", "cwd": "/made", "exec": "read=inside"} {
		if got[k] != want {
			t.Errorf("%s: %q, want %q", k, got[k], want)
		}
	}
	if !strings.Contains(got["list"], "etc,") || !strings.Contains(got["list"], "made,") || strings.Contains(got["list"], "Users,") {
		t.Errorf("/ lists %q, want the container's root", got["list"])
	}
	if _, err := os.Stat(filepath.Join(root, "made", "new")); err != nil {
		t.Errorf("the write did not land in the root: %v", err)
	}
}

func TestIdentityAndOwnership(t *testing.T) {
	root := containerRoot(t)
	got := probe(t, root, []string{"FWS_HOSTNAME=web-1"}, "ids", "user", "owner", "chown", "hostname")
	for k, want := range map[string]string{"ids": "0:0", "user": "root:/root", "owner": "0:0", "chown": "1000:50", "hostname": "web-1"} {
		if got[k] != want {
			t.Errorf("%s: %q, want %q", k, got[k], want)
		}
	}
	// The owner persists with the file, for the next process.
	if got := probe(t, root, nil, "owner"); got["owner"] != "1000:50" {
		t.Errorf("owner after chown, in a new process: %q", got["owner"])
	}
	// A user other than root sees itself, and may not give its files away.
	got = probe(t, root, []string{"FWS_UID=1000", "FWS_GID=50"}, "ids", "user", "write")
	if got["ids"] != "1000:50" || got["user"] != "app:/home/app" {
		t.Errorf("as app: ids %q, user %q", got["ids"], got["user"])
	}
	if got := probe(t, root, nil, "owner"); got["owner"] != "1000:50" {
		t.Errorf("owner: %q", got["owner"])
	}
}
