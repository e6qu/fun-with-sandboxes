package buildkit

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	ctdmetadata "github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/containerd/v2/plugins/content/local"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
)

func write(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func bind(dir string) []mount.Mount {
	return []mount.Mount{{Type: "bind", Source: dir, Options: []string{"rbind", "rw"}}}
}

// A layer the comparer writes, applied onto the lower tree, reproduces the
// upper tree: additions, changes and deletions.
func TestCompareThenApply(t *testing.T) {
	// The content store the worker uses: a local store behind containerd's
	// metadata database, which keeps the labels.
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
	cs := mdb.ContentStore()
	lower, upper, target := t.TempDir(), t.TempDir(), t.TempDir()
	for _, d := range []string{lower, upper, target} {
		write(t, d, "etc/keep", "same")
		write(t, d, "etc/change", "old")
		write(t, d, "gone", "bye")
	}
	write(t, upper, "etc/change", "new")
	write(t, upper, "added/file", "hello")
	os.Remove(filepath.Join(upper, "gone"))

	desc, err := comparer{store: cs}.Compare(ctx, bind(lower), bind(upper))
	if err != nil {
		t.Fatal(err)
	}
	if desc.MediaType != ocispec.MediaTypeImageLayerGzip || desc.Size == 0 {
		t.Fatalf("diff descriptor %+v", desc)
	}
	info, err := cs.Info(ctx, desc.Digest)
	if err != nil || info.Labels[labels.LabelUncompressed] == "" {
		t.Fatalf("diff info %+v, %v", info, err)
	}
	applied, err := applier{store: cs}.Apply(ctx, desc, bind(target))
	if err != nil {
		t.Fatal(err)
	}
	if applied.Digest.String() != info.Labels[labels.LabelUncompressed] {
		t.Errorf("applied digest %s, want the uncompressed digest %s", applied.Digest, info.Labels[labels.LabelUncompressed])
	}
	for name, want := range map[string]string{"etc/keep": "same", "etc/change": "new", "added/file": "hello"} {
		if b, _ := os.ReadFile(filepath.Join(target, name)); string(b) != want {
			t.Errorf("%s = %q, want %q", name, b, want)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "gone")); !os.IsNotExist(err) {
		t.Errorf("deleted file survived the applied diff: %v", err)
	}
}

func TestBindDirRefusesOtherMounts(t *testing.T) {
	if _, _, err := bindDir([]mount.Mount{{Type: "overlay", Source: "overlay"}}); err == nil {
		t.Fatal("an overlay mount was accepted")
	}
	dir, done, err := bindDir(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("no mounts did not yield an empty directory: %v", err)
	}
}
