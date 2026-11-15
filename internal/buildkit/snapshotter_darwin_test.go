package buildkit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A child snapshot is a clone of its parent: writes to it leave the
// committed parent untouched, and every snapshot is a read-write bind mount.
func TestSnapshotterClones(t *testing.T) {
	ctx := context.Background()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	sn, err := newSnapshotter(root)
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()
	ms, err := sn.Prepare(ctx, "base-active", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, ms[0].Source, "f", "base")
	if err := sn.Commit(ctx, "base", "base-active"); err != nil {
		t.Fatal(err)
	}
	child, err := sn.Prepare(ctx, "child", "base")
	if err != nil {
		t.Fatal(err)
	}
	if child[0].Type != "bind" || child[0].Options[1] != "rw" {
		t.Fatalf("child mount %+v", child[0])
	}
	write(t, child[0].Source, "f", "changed in child")
	view, err := sn.View(ctx, "view", "base")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(view[0].Source, "f")); string(b) != "base" {
		t.Fatalf("view of the parent reads %q after the child wrote", b)
	}
	if err := sn.Remove(ctx, "child"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(child[0].Source); !os.IsNotExist(err) {
		t.Fatalf("removed snapshot's directory survived: %v", err)
	}
}

// A snapshot's usage is what it holds that its parent does not.
func TestSnapshotterUsage(t *testing.T) {
	ctx := context.Background()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	sn, err := newSnapshotter(root)
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()
	ms, err := sn.Prepare(ctx, "base-active", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, ms[0].Source, "big", string(make([]byte, 4096)))
	if err := sn.Commit(ctx, "base", "base-active"); err != nil {
		t.Fatal(err)
	}
	child, err := sn.Prepare(ctx, "child-active", "base")
	if err != nil {
		t.Fatal(err)
	}
	write(t, child[0].Source, "small", "0123456789")
	if err := sn.Commit(ctx, "child", "child-active"); err != nil {
		t.Fatal(err)
	}
	if u, err := sn.Usage(ctx, "child"); err != nil || u.Size != 10 || u.Inodes != 1 {
		t.Errorf("the child's usage: %+v, %v; want its one file of 10 bytes", u, err)
	}
	if u, err := sn.Usage(ctx, "base"); err != nil || u.Size < 4096 {
		t.Errorf("the base's usage: %+v, %v; want its file of 4096 bytes", u, err)
	}
}

// The snapshots BuildKit unpacks an image's layers into each hold their
// layer alone, and a snapshot made on the last holds the whole tree, the
// lower layers' entries its whiteouts name gone.
func TestSnapshotterDeltas(t *testing.T) {
	ctx := context.Background()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	sn, err := newSnapshotter(root)
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()
	parent := ""
	for i, files := range []map[string]string{
		{"etc/passwd": "root", "etc/gone": "x", "big": string(make([]byte, 8192))},
		{"etc/.wh.gone": "", "nix/store/a/bin/tool": "tool"},
		{"nix/store/b/lib": "lib"},
	} {
		// As containerd's metadata names BuildKit's extraction snapshots.
		key := "buildkit/" + string(rune('1'+i)) + "/extract-id sha256:chain"
		ms, err := sn.Prepare(ctx, key, parent)
		if err != nil {
			t.Fatal(err)
		}
		if entries, _ := os.ReadDir(ms[0].Source); len(entries) != 0 {
			t.Fatalf("layer %d's snapshot starts with %d entries, want none", i, len(entries))
		}
		for name, body := range files {
			write(t, ms[0].Source, name, body)
		}
		parent = "layer-" + string(rune('1'+i))
		if err := sn.Commit(ctx, parent, key); err != nil {
			t.Fatal(err)
		}
	}
	// Its own file's blocks and directories, not the first layer's 8192
	// bytes below it.
	if u, err := sn.Usage(ctx, "layer-3"); err != nil || u.Size == 0 || u.Size >= 8192 {
		t.Errorf("the third layer's usage: %+v, %v; want its own file's alone", u, err)
	}
	for _, key := range []string{"view", "again"} {
		ms, err := sn.View(ctx, key, "layer-3")
		if err != nil {
			t.Fatal(err)
		}
		want := "big etc/ etc/passwd nix/ nix/store/ nix/store/a/ nix/store/a/bin/ nix/store/a/bin/tool nix/store/b/ nix/store/b/lib"
		var got []string
		filepath.WalkDir(ms[0].Source, func(p string, d os.DirEntry, err error) error {
			if err == nil && p != ms[0].Source {
				rel, _ := filepath.Rel(ms[0].Source, p)
				if d.IsDir() {
					rel += "/"
				}
				got = append(got, rel)
			}
			return err
		})
		if strings.Join(got, " ") != want {
			t.Errorf("%s of the layers:\n%s\nwant:\n%s", key, strings.Join(got, " "), want)
		}
	}
	// A snapshot on a middle layer holds the layers up to it.
	ms, err := sn.Prepare(ctx, "middle", "layer-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ms[0].Source, "nix/store/b")); !os.IsNotExist(err) {
		t.Errorf("a snapshot on the second layer holds the third's: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ms[0].Source, "nix/store/a/bin/tool")); string(b) != "tool" {
		t.Errorf("a snapshot on the second layer reads %q", b)
	}
}
