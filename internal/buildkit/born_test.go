package buildkit

import (
	"archive/tar"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	ctdmetadata "github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/plugins/content/local"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
)

// cloneSnapshot stands in for the snapshotter's clone where the tests run
// without APFS: it copies the snapshot src of root to dst with its times,
// and stamps dst born as the snapshotter does.
func cloneSnapshot(t *testing.T, root, src, dst string) string {
	t.Helper()
	from, to := filepath.Join(root, "snapshots", src), filepath.Join(root, "snapshots", dst)
	if err := exec.Command("cp", "-a", from, to).Run(); err != nil {
		t.Fatal(err)
	}
	// Inode change times are as coarse as the kernel's clock tick.
	time.Sleep(20 * time.Millisecond)
	if err := stampBorn(to, src); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	return to
}

// A file the clone made and nothing changed since is settled against the
// snapshot it was cloned from or another clone of it; one written since,
// or renamed into place, or compared with an unrelated tree, is not.
func TestSettledSince(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "snapshots", "1")
	at := time.Unix(1_600_000_000, 0)
	for _, n := range []string{"etc/a", "etc/b", "etc/c", "bin/x", "bin/y"} {
		write(t, parent, n, "same")
		os.Chtimes(filepath.Join(parent, n), at, at)
	}
	child := cloneSnapshot(t, root, "1", "2")
	view := cloneSnapshot(t, root, "1", "3")
	other := filepath.Join(root, "snapshots", "4")
	write(t, other, "etc/a", "same")

	// Rewritten in place with the same size and time.
	write(t, child, "etc/b", "diff")
	os.Chtimes(filepath.Join(child, "etc/b"), at, at)
	// Renamed over another file of the same size and time.
	if err := os.Rename(filepath.Join(child, "bin/x"), filepath.Join(child, "bin/y")); err != nil {
		t.Fatal(err)
	}

	stat := func(p string) os.FileInfo {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	settled := func(lower, upper, name string) bool {
		f := settledSince(lower, upper)
		if f == nil {
			return false
		}
		l, u := filepath.Join(lower, name), filepath.Join(upper, name)
		return f(stat(l), stat(u), stat(filepath.Dir(l)), stat(filepath.Dir(u)))
	}
	for _, c := range []struct {
		lower, upper, name string
		want               bool
	}{
		{parent, child, "etc/c", true},
		{view, child, "etc/c", true},
		{parent, child, "etc/b", false},
		{parent, child, "bin/y", false},
		// A write in place leaves the directory as it was.
		{parent, child, "etc/a", true},
		{other, child, "etc/a", false},
		{parent, view, "etc/a", true},
	} {
		if got := settled(c.lower, c.upper, c.name); got != c.want {
			t.Errorf("%s from %s to %s: settled %v, want %v", c.name, filepath.Base(c.lower), filepath.Base(c.upper), got, c.want)
		}
	}
	if settledSince(parent, filepath.Join(root, "snapshots", "9")) != nil {
		t.Error("a snapshot without a stamp has a settled check")
	}
}

// The comparer finds a file a step rewrote to the same size and time, and
// leaves out those the clone made.
func TestCompareFindsRewrittenFiles(t *testing.T) {
	ctx := withNS(context.Background())
	dir := t.TempDir()
	store, err := local.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(dir, "meta.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mdb := ctdmetadata.NewDB(db, store, nil)
	if err := mdb.Init(ctx); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	parent := filepath.Join(root, "snapshots", "1")
	at := time.Unix(1_600_000_000, 0)
	for _, n := range []string{"kept", "rewritten"} {
		write(t, parent, n, "same")
		os.Chtimes(filepath.Join(parent, n), at, at)
	}
	child := cloneSnapshot(t, root, "1", "2")
	write(t, child, "rewritten", "diff")
	os.Chtimes(filepath.Join(child, "rewritten"), at, at)

	desc, err := comparer{store: mdb.ContentStore()}.Compare(ctx, bind(parent), bind(child), diff.WithMediaType(ocispec.MediaTypeImageLayer))
	if err != nil {
		t.Fatal(err)
	}
	ra, err := mdb.ContentStore().ReaderAt(ctx, desc)
	if err != nil {
		t.Fatal(err)
	}
	defer ra.Close()
	var names []string
	tr := tar.NewReader(content.NewReader(ra))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		if h.Uid != 0 || h.Gid != 0 {
			t.Errorf("%s: owner %d:%d, want root", h.Name, h.Uid, h.Gid)
		}
	}
	if len(names) != 1 || names[0] != "rewritten" {
		t.Errorf("diff holds %v, want only rewritten", names)
	}
}

// BuildKit diffs views of two snapshots, clones made a moment before: the
// trees behind them are what the stamps tell of, the parent's own snapshot
// or, for a parent that holds one layer alone, its whole tree.
func TestBehindView(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "snapshots", "1")
	at := time.Unix(1_600_000_000, 0)
	for _, n := range []string{"etc/a", "etc/b"} {
		write(t, parent, n, "same")
		os.Chtimes(filepath.Join(parent, n), at, at)
	}
	child := cloneSnapshot(t, root, "1", "2")
	write(t, child, "etc/b", "diff")
	os.Chtimes(filepath.Join(child, "etc/b"), at, at)

	view := func(of, as string) string {
		dir := cloneSnapshot(t, root, of, as)
		if err := stampBorn(dir, viewOf+of); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	lower, upper := view("1", "3"), view("2", "4")
	if got := behindView(upper); got != child {
		t.Fatalf("behind the child's view: %s, want %s", got, child)
	}
	if got := behindView(lower); got != parent {
		t.Fatalf("behind the parent's view: %s, want %s", got, parent)
	}
	if got := behindView(child); got != child {
		t.Errorf("behind a snapshot that is no view: %s", got)
	}
	// The views alone tell nothing; the trees behind them do.
	if settledSince(lower, upper) != nil {
		t.Error("two views are settled against one another")
	}
	settled := settledSince(behindView(lower), behindView(upper))
	if settled == nil {
		t.Fatal("the trees behind the views are not settled")
	}
	stat := func(p string) os.FileInfo {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	for name, want := range map[string]bool{"etc/a": true, "etc/b": false} {
		got := settled(stat(filepath.Join(parent, name)), stat(filepath.Join(child, name)), stat(filepath.Join(parent, "etc")), stat(filepath.Join(child, "etc")))
		if got != want {
			t.Errorf("%s settled: %v, want %v", name, got, want)
		}
	}

	// A parent that holds a layer alone: its whole tree is behind its view.
	delta := filepath.Join(root, "snapshots", "5")
	write(t, delta, "only", "layer")
	os.MkdirAll(filepath.Dir(deltaMark(delta)), 0o755)
	os.WriteFile(deltaMark(delta), nil, 0o600)
	full := filepath.Join(root, "full", "5")
	write(t, full, "etc/a", "same")
	dv := filepath.Join(root, "snapshots", "6")
	write(t, dv, "etc/a", "same")
	if err := stampBorn(dv, viewOf+"5"); err != nil {
		t.Fatal(err)
	}
	if got := behindView(dv); got != full {
		t.Errorf("behind the view of a layer's snapshot: %s, want %s", got, full)
	}
}
