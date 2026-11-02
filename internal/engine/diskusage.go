package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/containerd/continuity/fs"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/volume"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// initDir holds an APFS clone of the container's root taken once create
// has set up its mounts: the state Docker's init layer holds, which docker
// diff and a container's size leave out.
func (c *Container) initDir() string { return filepath.Join(c.dir, "init") }

var errNoInit = errors.New("the container predates this engine's record of its initial root; recreate it")

// Changes lists what the container changed in its root, as docker diff
// does: additions, modifications and deletions since it was created, its
// mounts aside, which live outside the root.
func (e *Engine) Changes(ctx context.Context, ref string) ([]container.FilesystemChange, error) {
	c, err := e.Container(ref)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(c.initDir()); err != nil {
		return nil, unsupported("docker diff: %v", errNoInit)
	}
	out := []container.FilesystemChange{}
	err = fs.Changes(ctx, c.initDir(), c.root(), func(k fs.ChangeKind, p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		var kind container.ChangeType
		switch k {
		case fs.ChangeKindAdd:
			kind = container.ChangeAdd
		case fs.ChangeKindModify:
			kind = container.ChangeModify
		case fs.ChangeKindDelete:
			kind = container.ChangeDelete
		default:
			return nil
		}
		out = append(out, container.FilesystemChange{Path: p, Kind: kind})
		return nil
	})
	return out, err
}

// sizeRw is the bytes the container wrote to its root since create, as
// Docker reports a container's writable layer.
func (c *Container) sizeRw(ctx context.Context) (int64, error) {
	if _, err := os.Stat(c.initDir()); err != nil {
		return 0, unsupported("container size: %v", errNoInit)
	}
	u, err := fs.DiffUsage(ctx, c.initDir(), c.root())
	return u.Size, err
}

// Usage is what docker system df reports, in API 1.51's shape.
type Usage struct {
	LayersSize int64
	Images     []imagetypes.Summary
	Containers []container.Summary
	Volumes    []volume.Volume
	BuildCache []build.CacheRecord
}

// DiskUsage measures what each kind of object holds on disk, those kinds
// that want names: image, container, volume, build-cache. Images count
// their layers, each once however many images share it; containers their
// writes since create; volumes their files; the build cache the classic
// builder's steps.
func (e *Engine) DiskUsage(ctx context.Context, want map[string]bool) (Usage, error) {
	all := len(want) == 0
	var u Usage
	if all || want["image"] {
		u.LayersSize, u.Images = e.imageUsage()
	}
	if all || want["container"] {
		u.Containers = e.List(ListOptions{All: true, Size: true})
	}
	if all || want["volume"] {
		for _, v := range e.VolumeList(Filters{}) {
			size, err := dirUsage(ctx, v.Mountpoint)
			if err != nil {
				return u, err
			}
			v.UsageData = &volume.UsageData{Size: size, RefCount: int64(e.volumeRefs(v.Name))}
			u.Volumes = append(u.Volumes, v)
		}
	}
	if all || want["build-cache"] {
		records, err := e.buildCache().records()
		if err != nil {
			return u, err
		}
		u.BuildCache = records
	}
	return u, nil
}

// imageUsage sizes the images: each image's own size, the part of it in
// layers another image shares, and the containers using it; and the bytes
// of all layers, each once.
func (e *Engine) imageUsage() (int64, []imagetypes.Summary) {
	imgs := e.images.List()
	users := map[string]int{}
	layers := make([][]layerUse, len(imgs))
	for i, img := range imgs {
		layers[i] = e.layersOf(img)
		for _, l := range layers[i] {
			users[l.digest]++
		}
	}
	counted := map[string]bool{}
	var total int64
	out := make([]imagetypes.Summary, 0, len(imgs))
	for i, img := range imgs {
		var shared int64
		for _, l := range layers[i] {
			if users[l.digest] > 1 {
				shared += l.size
			}
			if !counted[l.digest] {
				counted[l.digest] = true
				total += l.size
			}
		}
		tags, digests := nonNil(img.RepoTags), nonNil(img.RepoDigests)
		labels := img.Config.Config.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		out = append(out, imagetypes.Summary{
			ID: img.ID, Created: img.Created.Unix(), RepoTags: tags, RepoDigests: digests,
			Size: img.Size, SharedSize: shared, Labels: labels, Containers: int64(e.imageUsers(img.ID)),
		})
	}
	return total, out
}

type layerUse struct {
	digest string
	size   int64
}

// layersOf pairs an image's layer digests with their uncompressed sizes.
func (e *Engine) layersOf(img image.Image) []layerUse {
	v, _, err := e.images.V1Image(img.ID)
	if err != nil {
		return nil
	}
	m, err := v.Manifest()
	if err != nil || len(m.Layers) != len(img.LayerSizes) {
		return nil
	}
	out := make([]layerUse, len(m.Layers))
	for i, l := range m.Layers {
		out[i] = layerUse{digest: l.Digest.String(), size: img.LayerSizes[i]}
	}
	return out
}

func (e *Engine) imageUsers(id string) int {
	n := 0
	for _, c := range e.allContainers() {
		if c.rec.ImageID == id {
			n++
		}
	}
	return n
}

func (e *Engine) volumeRefs(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.containers {
		for _, m := range c.rec.Mounts {
			if m.Name == name {
				n++
			}
		}
	}
	return n
}

func dirUsage(ctx context.Context, dir string) (int64, error) {
	u, err := fs.DiskUsage(ctx, dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return u.Size, err
}

// records lists the classic builder's cached steps as build cache records:
// one per step that left a layer, sized by it.
func (c *buildCache) records() ([]build.CacheRecord, error) {
	entries, err := os.ReadDir(c.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []build.CacheRecord
	for _, de := range entries {
		key, ok := strings.CutSuffix(de.Name(), ".json")
		if !ok {
			continue
		}
		e, ok := c.get(key)
		if !ok || e.Layer == "" {
			continue
		}
		fi, err := os.Stat(c.layerPath(e))
		if err != nil {
			continue
		}
		out = append(out, build.CacheRecord{
			ID: key, Type: "regular", Description: "classic builder step", Size: fi.Size(), CreatedAt: e.Created,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
