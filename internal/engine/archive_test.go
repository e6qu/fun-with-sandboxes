package engine

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/moby/moby/api/types/mount"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// newView builds a container root with a volume mounted at /data, a relative
// symlink, and a symlink pointing at a host path outside the container.
func newView(t *testing.T) (fsView, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, vol, host := filepath.Join(base, "r"), filepath.Join(base, "vol"), filepath.Join(base, "host")
	for _, d := range []string{filepath.Join(root, "etc"), vol, host} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(root, "etc", "conf"), []byte("conf"), 0o644)
	os.WriteFile(filepath.Join(vol, "row"), []byte("row"), 0o644)
	os.WriteFile(filepath.Join(host, "secret"), []byte("secret"), 0o600)
	os.Symlink("etc/conf", filepath.Join(root, "conf-link"))
	os.Symlink(host, filepath.Join(root, "escape"))
	os.Symlink(vol, filepath.Join(root, "data"))
	return fsView{root: root, mounts: []mountRec{{Type: mount.TypeVolume, Name: "v", Source: vol, Destination: "/data", RW: true}}}, host
}

func tarNames(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = string(body) + h.Linkname
	}
}

func TestResolveConfinesToTheContainer(t *testing.T) {
	v, host := newView(t)
	if h, err := v.resolve("/etc/conf", false); err != nil || h != filepath.Join(v.root, "etc", "conf") {
		t.Errorf("resolve /etc/conf = %q, %v", h, err)
	}
	if h, err := v.resolve("/data/row", false); err != nil || h != filepath.Join(v.mounts[0].Source, "row") {
		t.Errorf("resolve /data/row = %q, %v; want the volume", h, err)
	}
	if h, err := v.resolve("/conf-link", true); err != nil || h != filepath.Join(v.root, "etc", "conf") {
		t.Errorf("resolve /conf-link following = %q, %v", h, err)
	}
	for _, p := range []string{"/escape/secret", "/escape", "/../../host/secret"} {
		if h, err := v.resolve(p, true); err == nil {
			t.Errorf("resolve %s = %q, want refusal: it leads to %s on the host", p, h, host)
		}
	}
	if _, err := v.resolve("/missing", false); KindOf(err) != KindNotFound {
		t.Errorf("missing path: %v", err)
	}
}

func TestArchiveCopiesMountContents(t *testing.T) {
	v, _ := newView(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	h, _ := v.resolve("/", false)
	if err := v.writeTree(tw, h, "r"); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	got := tarNames(t, buf.Bytes())
	if got["r/data/row"] != "row" {
		t.Errorf("volume content missing from the archive: %v", keys(got))
	}
	if _, ok := got["r/data"]; ok {
		t.Errorf("the mount symlink itself was archived")
	}
	if got["r/conf-link"] != "etc/conf" {
		t.Errorf("relative symlink not archived as a link: %q", got["r/conf-link"])
	}
	st, _, err := v.stat("/data")
	if err != nil || !st.Mode.IsDir() || st.Name != "data" {
		t.Errorf("stat /data = %+v, %v; want the mounted directory", st, err)
	}
	st, _, err = v.stat("/conf-link")
	if err != nil || st.LinkTarget != "/etc/conf" {
		t.Errorf("stat /conf-link = %+v, %v", st, err)
	}
}

func TestExtractIntoContainer(t *testing.T) {
	v, _ := newView(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "init.sql", Mode: 0o644, Size: 7, Typeflag: tar.TypeReg})
	tw.Write([]byte("select;"))
	tw.WriteHeader(&tar.Header{Name: ".wh.keep", Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Close()
	h, err := v.resolve("/data", true)
	if err != nil || !v.writable(h) {
		t.Fatalf("resolve /data: %q, %v", h, err)
	}
	if err := image.Extract(h, &buf, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(v.mounts[0].Source, "init.sql")); string(b) != "select;" {
		t.Errorf("file not extracted into the volume: %q", b)
	}
	if _, err := os.Stat(filepath.Join(v.mounts[0].Source, ".wh.keep")); err != nil {
		t.Errorf(".wh. file treated as a whiteout: %v", err)
	}
	ro := v
	ro.mounts = []mountRec{{Type: mount.TypeVolume, Source: v.mounts[0].Source, Destination: "/data"}}
	if ro.writable(h) {
		t.Error("read-only mount reported writable")
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
