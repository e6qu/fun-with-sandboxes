package buildkit

import (
	"context"
	"os"
	"path/filepath"
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
