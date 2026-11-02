//go:build e2e && darwin

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A local volume with o=bind binds a host directory, as Compose's
// driver_opts { type: none, o: bind, device: … } asks.
func TestBindVolume(t *testing.T) {
	home, _ := os.UserHomeDir()
	host, err := os.MkdirTemp(home, ".bindvol")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(host)
	if err := os.WriteFile(filepath.Join(host, "seed"), []byte("from the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const vol = "fws-e2e-bindvol"
	mustDocker(t, "volume", "create", "--opt", "type=none", "--opt", "o=bind", "--opt", "device="+host, vol)
	defer docker(t, "volume", "rm", "-f", vol)
	if s := strings.TrimSpace(mustDocker(t, "volume", "inspect", "-f", "{{.Options.device}}", vol)); s != host {
		t.Fatalf("volume options show device %q", s)
	}
	if out := mustDocker(t, "run", "--rm", "-v", vol+":/data", "fws/toolbox:1", "cat", "/data/seed"); out != "from the host\n" {
		t.Fatalf("the container read %q from the bound directory", out)
	}
	mustDocker(t, "run", "--rm", "-v", vol+":/data", "fws/toolbox:1", "put", "/data/written", "by the container")
	if b, err := os.ReadFile(filepath.Join(host, "written")); err != nil || string(b) != "by the container\n" {
		t.Fatalf("the host directory holds %q, %v", b, err)
	}
	if _, err := docker(t, "volume", "create", "--opt", "type=nfs", "--opt", "device=:/x", "fws-e2e-nfs"); err == nil || !strings.Contains(err.Error(), "needs root") {
		t.Fatalf("an nfs volume: %v", err)
	}
}

// A tmpfs volume starts empty each time a container first mounts it.
func TestTmpfsVolume(t *testing.T) {
	const vol = "fws-e2e-tmpfsvol"
	mustDocker(t, "volume", "create", "--opt", "type=tmpfs", "--opt", "device=tmpfs", vol)
	defer docker(t, "volume", "rm", "-f", vol)
	mustDocker(t, "run", "--rm", "-v", vol+":/t", "fws/toolbox:1", "put", "/t/f", "x")
	if _, err := docker(t, "run", "--rm", "-v", vol+":/t", "fws/toolbox:1", "cat", "/t/f"); err == nil {
		t.Fatal("a tmpfs volume kept a file across mounts")
	}
}

func TestVolumeSubpathAndNoCopy(t *testing.T) {
	const vol = "fws-e2e-subvol"
	mustDocker(t, "run", "--rm", "-v", vol+":/v", "fws/toolbox:1", "put", "/v/inner/f", "inside")
	defer docker(t, "volume", "rm", "-f", vol)
	if out := mustDocker(t, "run", "--rm", "--mount", "type=volume,src="+vol+",dst=/sub,volume-subpath=inner", "fws/toolbox:1", "cat", "/sub/f"); out != "inside\n" {
		t.Fatalf("the subpath mount shows %q", out)
	}
	if _, err := docker(t, "run", "--rm", "--mount", "type=volume,src="+vol+",dst=/sub,volume-subpath=missing", "fws/toolbox:1", "echo"); err == nil {
		t.Fatal("a missing subpath mounted")
	}
	// Without nocopy a new volume takes the image's content at its mount
	// point; with it the volume starts empty.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(`FROM fws/toolbox:1
RUN ["/toolbox", "put", "/seeded/f", "from the image"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	classicBuild(t, "-t", "e2e/seeded:1", dir)
	defer docker(t, "rmi", "e2e/seeded:1")
	const empty, copied = "fws-e2e-nocopy", "fws-e2e-copyup"
	mustDocker(t, "run", "--rm", "--mount", "type=volume,src="+empty+",dst=/seeded,volume-nocopy", "e2e/seeded:1", "echo")
	defer docker(t, "volume", "rm", "-f", empty)
	mustDocker(t, "run", "--rm", "--mount", "type=volume,src="+copied+",dst=/seeded", "e2e/seeded:1", "echo")
	defer docker(t, "volume", "rm", "-f", copied)
	list := func(vol string) string {
		return strings.TrimSpace(mustDocker(t, "run", "--rm", "-v", vol+":/look", "fws/toolbox:1", "ls", "/look"))
	}
	if got := list(empty); got != "" {
		t.Errorf("a nocopy volume holds %q", got)
	}
	if got := list(copied); got != "f" {
		t.Errorf("a new volume holds %q, want the image's f", got)
	}
}

// An image mount shows another image's root, read-only, and keeps the
// image from removal.
func TestImageMount(t *testing.T) {
	mustDocker(t, "tag", "fws/toolbox:1", "e2e/mounted:1")
	defer docker(t, "rmi", "-f", "e2e/mounted:1")
	const name = "fws-e2e-imgmount"
	mustDocker(t, "run", "-d", "--name", name, "--mount", "type=image,src=e2e/mounted:1,dst=/img", "fws/toolbox:1", "hold")
	defer docker(t, "rm", "-f", name)
	if out := mustDocker(t, "exec", name, "/toolbox", "ls", "/img"); !strings.Contains(out, "toolbox") {
		t.Fatalf("the image mount lists %q", out)
	}
	if _, err := docker(t, "exec", name, "/toolbox", "put", "/img/written", "x"); err == nil {
		t.Fatal("a container wrote into an image mount")
	}
	if s := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{range .Mounts}}{{.Type}} {{.RW}}{{end}}", name)); s != "image false" {
		t.Fatalf("inspect shows the mount as %q", s)
	}
}
