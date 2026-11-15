package buildkit

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	ctdmetadata "github.com/containerd/containerd/v2/core/metadata"
	ctdsnapshot "github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	bolt "go.etcd.io/bbolt"

	containerdsnapshot "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/snapshot/containerd"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/leaseutil"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// The copy of an engine image BuildKit holds goes once the engine deletes
// the image: its record and the blobs only it referred to.
func TestRemovedImagesLeaveBuildKit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine, err := image.Open(filepath.Join(dir, "images"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(64, 2)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.DeepCopy()
	cfg.OS, cfg.Architecture = image.Platform.OS, image.Platform.Architecture
	if img, err = mutate.ConfigFile(img, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Add(img, []string{"base:1"}, ""); err != nil {
		t.Fatal(err)
	}

	store, err := local.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(dir, "meta.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mdb := ctdmetadata.NewDB(db, store, map[string]ctdsnapshot.Snapshotter{})
	if err := mdb.Init(ctx); err != nil {
		t.Fatal(err)
	}
	cs := containerdsnapshot.NewContentStore(mdb.ContentStore(), namespace)
	l := &localImages{
		engine: engine, meta: ctdmetadata.NewImageStore(mdb), content: cs,
		leases: leaseutil.WithNamespace(ctdmetadata.NewLeaseManager(mdb), namespace),
		gc: func(ctx context.Context) error {
			_, err := mdb.GarbageCollect(ctx)
			return err
		},
	}
	defer engine.OnRemove(func(id string) {
		if err := l.forget(ctx, id); err != nil {
			t.Error(err)
		}
	})()
	if _, err := l.Get(ctx, "docker.io/library/base:1"); err != nil {
		t.Fatal(err)
	}
	blobs := func() int {
		n := 0
		if err := cs.Walk(withNS(ctx), func(content.Info) error { n++; return nil }); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The manifest, the config and two layers.
	if n := blobs(); n != 4 {
		t.Fatalf("BuildKit holds %d blobs, want 4", n)
	}
	if _, err := engine.Remove("base:1", false, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if recs, err := l.meta.List(withNS(ctx)); err != nil || len(recs) != 0 {
		t.Fatalf("records after the engine removed the image: %v %v", recs, err)
	}
	if n := blobs(); n != 0 {
		t.Fatalf("BuildKit holds %d blobs after the engine removed the image", n)
	}
}
