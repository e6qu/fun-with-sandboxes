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
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/continuity/fs"
)

// SnapshotterName is how BuildKit and containerd's metadata refer to the
// snapshotter.
const SnapshotterName = "apfs"

// snapshotter keeps each snapshot as a directory. A new snapshot is an APFS
// clone of its parent, so preparing one costs no copying, and every
// snapshot, views included, is its own directory handed out as a read-write
// bind mount: BuildKit's darwin mounter uses such a mount's source as is,
// where any other mount would need mount(2) and root.
//
// A clone of a tree still costs the kernel a visit to each of its files,
// seconds for a large image, and BuildKit unpacks a base image a layer at
// a time, each into a snapshot of the one below: cloning the whole tree
// so far for each of an image's dozens of layers took minutes. So a
// snapshot BuildKit prepares to unpack a layer into is a delta: its
// directory holds that layer's entries alone, whiteouts as the files the
// layer names them by, and a mark says so. The whole tree of a delta is
// put together once, the first time a snapshot is made on it, from the
// deltas below it in order (full), and cloned from there after.
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

// deltaMark is the file that marks the snapshot whose directory is dir as
// a delta, and fullDir where that snapshot's whole tree is kept.
func deltaMark(dir string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(dir)), "delta", filepath.Base(dir))
}

func isDelta(dir string) bool {
	_, err := os.Lstat(deltaMark(dir))
	return err == nil
}

func (o *snapshotter) fullDir(id string) string { return filepath.Join(o.root, "full", id) }

// extracts reports whether key is of a snapshot BuildKit unpacks a layer
// into (cache.unlazyLayer names it extract-<id> <chain>), as containerd's
// metadata hands it on: after the namespace and an ID of its own.
func extracts(key string) bool {
	return strings.HasPrefix(key[strings.LastIndex(key, "/")+1:], "extract-")
}

// full is the directory holding the whole tree of the snapshot ids[0],
// whose ancestors follow it, nearest first: its own, or for a delta the
// tree put together from the nearest ancestor that has one and the deltas
// above it.
func (o *snapshotter) full(ids []string) (string, error) {
	if !isDelta(o.dir(ids[0])) {
		return o.dir(ids[0]), nil
	}
	dst := o.fullDir(ids[0])
	if _, err := os.Lstat(dst); err == nil {
		return dst, nil
	}
	n := 0
	for n < len(ids) && isDelta(o.dir(ids[n])) {
		if _, err := os.Lstat(o.fullDir(ids[n])); err == nil && n > 0 {
			break
		}
		n++
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dst), "tmp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	tree := filepath.Join(tmp, "tree")
	switch {
	case n == len(ids):
		err = os.Mkdir(tree, 0o755)
	case isDelta(o.dir(ids[n])):
		err = cloneTree(o.fullDir(ids[n]), tree)
	default:
		err = cloneTree(o.dir(ids[n]), tree)
	}
	for i := n - 1; i >= 0 && err == nil; i-- {
		err = mergeDelta(o.dir(ids[i]), tree, cloneTree)
	}
	if err != nil {
		return "", err
	}
	return dst, os.Rename(tree, dst)
}

// mergeDelta lays the layer whose entries delta holds over the tree at
// dst: what its whiteouts name goes from dst first, as they hide the
// layers below and nothing of their own layer, and its entries follow. A
// directory dst lacks comes as one clone of the layer's whole subtree.
func mergeDelta(delta, dst string, clone func(src, dst string) error) error {
	const whiteout, opaque = ".wh.", ".wh..wh..opq"
	var marks []string
	err := filepath.WalkDir(delta, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == delta || !strings.HasPrefix(d.Name(), whiteout) {
			return err
		}
		rel, err := filepath.Rel(delta, p)
		if err != nil {
			return err
		}
		marks = append(marks, rel)
		dir := filepath.Join(dst, filepath.Dir(rel))
		if d.Name() != opaque {
			return os.RemoveAll(filepath.Join(dir, strings.TrimPrefix(d.Name(), whiteout)))
		}
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return filepath.WalkDir(delta, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == delta {
			return err
		}
		if strings.HasPrefix(d.Name(), whiteout) {
			return nil
		}
		rel, err := filepath.Rel(delta, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		fi, lerr := os.Lstat(target)
		if d.IsDir() && lerr == nil && fi.IsDir() {
			return copyDirMeta(p, target)
		}
		if lerr == nil {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
		if err := clone(p, target); err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		// The subtree came whole, the marks inside it too.
		for _, m := range marks {
			if strings.HasPrefix(m, rel+string(filepath.Separator)) {
				if err := os.Remove(filepath.Join(dst, m)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
		return filepath.SkipDir
	})
}

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

// Usage measures what a snapshot holds that its parent does not, as
// containerd defines a snapshot's usage, when BuildKit asks for it.
func (o *snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	var id, parentID string
	err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		var info snapshots.Info
		var err error
		if id, info, _, err = storage.GetInfo(ctx, key); err != nil || info.Parent == "" {
			return err
		}
		parentID, _, _, err = storage.GetInfo(ctx, info.Parent)
		return err
	})
	if err != nil {
		return snapshots.Usage{}, err
	}
	// A delta holds its layer alone.
	if parentID == "" || isDelta(o.dir(id)) {
		du, err := fs.DiskUsage(ctx, o.dir(id))
		return snapshots.Usage(du), err
	}
	return addedUsage(o.dir(parentID), o.dir(id))
}

// addedUsage counts the entries of child that parent, the tree it was
// cloned from, does not hold as they are: a file or link of another size,
// modification time or mode, a directory of another mode, and whatever
// parent lacks. It reads no file's content, where continuity's fs.Changes
// compares the content of files whose times are whole seconds, as every
// file of a layer unpacked from a tar is: BuildKit asks for the usage of
// each layer of a base image, and each call read the image below it twice.
// A file rewritten in place to the same size within its second counts as
// unchanged, which misstates a size BuildKit prunes its cache by, and no
// layer's content.
func addedUsage(parent, child string) (snapshots.Usage, error) {
	var u snapshots.Usage
	err := filepath.WalkDir(child, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == child {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(child, p)
		if err != nil {
			return err
		}
		if was, err := os.Lstat(filepath.Join(parent, rel)); err == nil && was.Mode() == fi.Mode() &&
			(fi.IsDir() || (was.Size() == fi.Size() && was.ModTime().Equal(fi.ModTime()))) {
			return nil
		}
		u.Inodes++
		if fi.Mode().IsRegular() {
			u.Size += fi.Size()
		}
		return nil
	})
	return u, err
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
		_, err := storage.CommitActive(ctx, key, name, snapshots.Usage{}, opts...)
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
	os.Remove(deltaMark(o.dir(id)))
	if err := os.RemoveAll(o.fullDir(id)); err != nil {
		return err
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
		if kind == snapshots.KindActive && extracts(key) {
			if err := os.MkdirAll(filepath.Dir(deltaMark(dst)), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(deltaMark(dst), nil, 0o600); err != nil {
				return err
			}
			return os.Mkdir(dst, 0o755)
		}
		if len(s.ParentIDs) > 0 {
			src, err := o.full(s.ParentIDs)
			if err != nil {
				return err
			}
			return cloneTree(src, dst)
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
