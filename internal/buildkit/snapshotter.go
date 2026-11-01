// Package buildkit runs the real BuildKit solver on a darwin worker: an APFS
// clone snapshotter, layer diffs over plain directories, and an executor
// that runs RUN steps under Seatbelt. It serves BuildKit's control API and
// sessions as dockerd does, so buildx and Compose drive it unchanged.
package buildkit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/continuity/fs"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// SnapshotterName is how BuildKit and containerd's metadata refer to the
// snapshotter.
const SnapshotterName = "apfs"

// snapshotter keeps each snapshot as a directory. A new snapshot is an APFS
// clone of its parent, so preparing one costs no copying, and every
// snapshot, views included, is its own directory handed out as a read-write
// bind mount: BuildKit's darwin mounter uses such a mount's source as is,
// where any other mount would need mount(2) and root.
type snapshotter struct {
	root string
	ms   *storage.MetaStore
}

func newSnapshotter(root string) (snapshots.Snapshotter, error) {
	if err := os.MkdirAll(filepath.Join(root, "snapshots"), 0o700); err != nil {
		return nil, err
	}
	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, err
	}
	return &snapshotter{root: root, ms: ms}, nil
}

func (o *snapshotter) dir(id string) string { return filepath.Join(o.root, "snapshots", id) }

func (o *snapshotter) Stat(ctx context.Context, key string) (info snapshots.Info, err error) {
	err = o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, info, _, err = storage.GetInfo(ctx, key)
		return err
	})
	return info, err
}

func (o *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (_ snapshots.Info, err error) {
	err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
		return err
	})
	return info, err
}

func (o *snapshotter) Usage(ctx context.Context, key string) (usage snapshots.Usage, err error) {
	var id string
	var info snapshots.Info
	err = o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, info, usage, err = storage.GetInfo(ctx, key)
		return err
	})
	if err != nil {
		return snapshots.Usage{}, err
	}
	if info.Kind == snapshots.KindActive {
		du, err := fs.DiskUsage(ctx, o.dir(id))
		if err != nil {
			return snapshots.Usage{}, err
		}
		usage = snapshots.Usage(du)
	}
	return usage, nil
}

func (o *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return o.create(ctx, snapshots.KindActive, key, parent, opts)
}

func (o *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return o.create(ctx, snapshots.KindView, key, parent, opts)
}

func (o *snapshotter) Mounts(ctx context.Context, key string) (_ []mount.Mount, err error) {
	var s storage.Snapshot
	err = o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		s, err = storage.GetSnapshot(ctx, key)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("get snapshot mounts: %w", err)
	}
	return o.mounts(s), nil
}

func (o *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, _, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		du, err := fs.DiskUsage(ctx, o.dir(id))
		if err != nil {
			return err
		}
		_, err = storage.CommitActive(ctx, key, name, snapshots.Usage(du), opts...)
		return err
	})
}

func (o *snapshotter) Remove(ctx context.Context, key string) error {
	var id string
	err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		var err error
		id, _, err = storage.Remove(ctx, key)
		return err
	})
	if err != nil {
		return fmt.Errorf("remove snapshot %s: %w", key, err)
	}
	return os.RemoveAll(o.dir(id))
}

func (o *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	return o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, fn, filters...)
	})
}

func (o *snapshotter) Close() error { return o.ms.Close() }

func (o *snapshotter) create(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {
	var s storage.Snapshot
	err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		s, err = storage.CreateSnapshot(ctx, kind, key, parent, opts...)
		if err != nil {
			return err
		}
		dst := o.dir(s.ID)
		if len(s.ParentIDs) > 0 {
			return image.CloneDir(o.dir(s.ParentIDs[0]), dst)
		}
		return os.Mkdir(dst, 0o755)
	})
	if err != nil {
		return nil, fmt.Errorf("create snapshot %s: %w", key, err)
	}
	return o.mounts(s), nil
}

func (o *snapshotter) mounts(s storage.Snapshot) []mount.Mount {
	return []mount.Mount{{Type: "bind", Source: o.dir(s.ID), Options: []string{"rbind", "rw"}}}
}
