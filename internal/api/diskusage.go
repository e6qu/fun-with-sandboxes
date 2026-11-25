package api

import (
	"net/http"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/volume"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// diskUsage is GET /system/df's answer from API 1.52: each kind's counts,
// total and reclaimable bytes, with its items where the request is
// verbose, beside the legacy lists, which a verbose request leaves out, as
// dockerd does until 1.53 drops them.
type diskUsage struct {
	LayersSize int64                `json:",omitempty"`
	Images     []imagetypes.Summary `json:",omitempty"`
	Containers []container.Summary  `json:",omitempty"`
	Volumes    []volume.Volume      `json:",omitempty"`
	BuildCache []build.CacheRecord  `json:",omitempty"`

	ImageUsage      *imagetypes.DiskUsage `json:",omitempty"`
	ContainerUsage  *container.DiskUsage  `json:",omitempty"`
	VolumeUsage     *volume.DiskUsage     `json:",omitempty"`
	BuildCacheUsage *build.DiskUsage      `json:",omitempty"`
}

// diskUsageFor shapes u for a request of API 1.52 or later; want holds the
// kinds the request asked for, every kind where it is empty.
func diskUsageFor(r *http.Request, u engine.Usage, want map[string]bool) diskUsage {
	verbose := boolParam(r.URL.Query().Get("verbose"))
	all := len(want) == 0
	var d diskUsage
	if !verbose {
		d.LayersSize, d.Images, d.Containers, d.Volumes, d.BuildCache = u.LayersSize, u.Images, u.Containers, u.Volumes, u.BuildCache
	}
	if all || want["image"] {
		iu := &imagetypes.DiskUsage{TotalCount: int64(len(u.Images)), TotalSize: u.LayersSize}
		for _, img := range u.Images {
			if img.Containers > 0 {
				iu.ActiveCount++
			} else if img.Size > img.SharedSize {
				// What only an unused image holds goes with it.
				iu.Reclaimable += img.Size - max(img.SharedSize, 0)
			}
		}
		if verbose {
			iu.Items = u.Images
		}
		d.ImageUsage = iu
	}
	if all || want["container"] {
		cu := &container.DiskUsage{TotalCount: int64(len(u.Containers))}
		for _, c := range u.Containers {
			cu.TotalSize += c.SizeRw
			if c.State == container.StateRunning {
				cu.ActiveCount++
			} else {
				cu.Reclaimable += c.SizeRw
			}
		}
		if verbose {
			cu.Items = u.Containers
		}
		d.ContainerUsage = cu
	}
	if all || want["volume"] {
		vu := &volume.DiskUsage{TotalCount: int64(len(u.Volumes))}
		for _, v := range u.Volumes {
			if v.UsageData == nil {
				continue
			}
			size := max(v.UsageData.Size, 0)
			vu.TotalSize += size
			if v.UsageData.RefCount > 0 {
				vu.ActiveCount++
			} else {
				vu.Reclaimable += size
			}
		}
		if verbose {
			vu.Items = u.Volumes
		}
		d.VolumeUsage = vu
	}
	if all || want["build-cache"] {
		bu := &build.DiskUsage{TotalCount: int64(len(u.BuildCache))}
		for _, c := range u.BuildCache {
			if !c.Shared {
				bu.TotalSize += c.Size
			}
			if c.InUse {
				bu.ActiveCount++
			} else if !c.Shared {
				bu.Reclaimable += c.Size
			}
		}
		if verbose {
			bu.Items = u.BuildCache
		}
		d.BuildCacheUsage = bu
	}
	return d
}
