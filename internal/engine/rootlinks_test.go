package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// A link in a container's root leads on inside the root, as the container
// sees it, when the engine writes there: its absolute target names a host
// directory the engine's writes must never reach.
func TestWritesInARootStayInIt(t *testing.T) {
	dir := t.TempDir()
	root, host, ctx := filepath.Join(dir, "root"), filepath.Join(dir, "host"), filepath.Join(dir, "ctx")
	for _, d := range []string{root, host, filepath.Join(ctx, "out/sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ctx, "out/sub/f"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{"etc", "out"} {
		if err := os.Symlink(host, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := placeMount(root, "/out/m", dir); err != nil {
		t.Fatal(err)
	}
	if err := writeHostsFiles(root, "box", "", nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := copyInto(root, ctx, root, 0); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(host); err != nil || len(entries) > 0 {
		t.Fatalf("the engine wrote to the host through the root's links: %v, %v", entries, err)
	}
	in := filepath.Join(root, host)
	for _, p := range []string{"m", "hosts", "hostname", "sub/f"} {
		if _, err := os.Lstat(filepath.Join(in, p)); err != nil {
			t.Errorf("%s in the root: %v", p, err)
		}
	}
}
