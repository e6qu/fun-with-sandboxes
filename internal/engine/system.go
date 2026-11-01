package engine

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/system"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

const (
	Version       = "0.1.0"
	APIVersion    = "1.51"
	MinAPIVersion = "1.24"
)

func (e *Engine) Version() system.VersionResponse {
	return system.VersionResponse{
		Platform:      system.PlatformInfo{Name: "fun-with-sandboxes"},
		Version:       Version,
		APIVersion:    APIVersion,
		MinAPIVersion: MinAPIVersion,
		Os:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		GoVersion:     runtime.Version(),
		KernelVersion: kernelVersion(),
		Components: []system.ComponentVersion{{
			Name: "Engine", Version: Version,
			Details: map[string]string{"ApiVersion": APIVersion, "MinAPIVersion": MinAPIVersion, "Os": runtime.GOOS, "Arch": runtime.GOARCH},
		}},
	}
}

func kernelVersion() string {
	out, err := exec.Command("/usr/bin/uname", "-r").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func productVersion() string {
	out, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (e *Engine) Info() system.Info {
	var running, stopped, paused int
	all := e.allContainers()
	for _, c := range all {
		c.mu.Lock()
		switch c.stateName() {
		case container.StateRunning:
			running++
		case container.StatePaused:
			paused++
		default:
			stopped++
		}
		c.mu.Unlock()
	}
	host, _ := os.Hostname()
	return system.Info{
		ID: "fun-with-sandboxes", Containers: len(all), ContainersRunning: running, ContainersPaused: paused, ContainersStopped: stopped,
		Images: len(e.images.List()), Driver: "apfs-clone", LoggingDriver: "json-file",
		KernelVersion: kernelVersion(), OperatingSystem: "macOS " + productVersion(), OSVersion: productVersion(),
		OSType: runtime.GOOS, Architecture: runtime.GOARCH, NCPU: runtime.NumCPU(), MemTotal: int64(memTotal()),
		DockerRootDir: e.cfg.Root, Name: host, ServerVersion: Version, IndexServerAddress: "https://index.docker.io/v1/",
		SystemTime: time.Now().Format(time.RFC3339Nano), DefaultRuntime: "seatbelt",
		SecurityOptions: []string{"name=seatbelt"}, Labels: []string{}, Warnings: []string{},
	}
}

func (e *Engine) imageInUse(id string) bool {
	for _, c := range e.allContainers() {
		if c.rec.ImageID == id {
			return true
		}
	}
	return false
}

func (e *Engine) ImageList(f Filters) []imagetypes.Summary {
	out := []imagetypes.Summary{}
	for _, img := range e.images.List() {
		if !f.matchLabels(img.Config.Config.Labels) {
			continue
		}
		if f.has("reference") {
			ok := false
			for _, want := range f["reference"] {
				for _, t := range append(append([]string(nil), img.RepoTags...), img.RepoDigests...) {
					if matchReference(want, t) {
						ok = true
					}
				}
			}
			if !ok {
				continue
			}
		}
		if dangling, ok := f.boolValue("dangling"); ok && dangling != isDangling(img) {
			continue
		}
		tags := img.RepoTags
		if tags == nil {
			tags = []string{}
		}
		labels := img.Config.Config.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		out = append(out, imagetypes.Summary{
			ID: img.ID, Created: img.Created.Unix(), RepoTags: tags, RepoDigests: nonNil(img.RepoDigests),
			Size: img.Size, Labels: labels, Containers: -1, SharedSize: -1,
		})
	}
	return out
}

// matchReference matches a reference filter, which may omit the tag, against
// a stored tag.
func matchReference(pattern, ref string) bool {
	if ok, _ := path.Match(pattern, ref); ok {
		return true
	}
	ok, _ := path.Match(pattern, image.Repository(ref))
	return ok
}

func (e *Engine) ImageInspect(ref string) (imagetypes.InspectResponse, error) {
	img, err := e.images.Get(ref)
	if err != nil {
		return imagetypes.InspectResponse{}, notFound("No such image: %s", ref)
	}
	var cfg dockerspec.DockerOCIImageConfig
	b, _ := json.Marshal(img.Config.Config)
	json.Unmarshal(b, &cfg)
	layers := make([]string, 0, len(img.Config.RootFS.DiffIDs))
	for _, d := range img.Config.RootFS.DiffIDs {
		layers = append(layers, d.String())
	}
	tags := img.RepoTags
	if tags == nil {
		tags = []string{}
	}
	return imagetypes.InspectResponse{
		ID: img.ID, RepoTags: tags, RepoDigests: nonNil(img.RepoDigests), Created: img.Created.Format(time.RFC3339Nano),
		Author: img.Config.Author, Config: &cfg, Architecture: img.Config.Architecture, Os: img.Config.OS,
		Variant: img.Config.Variant, Size: img.Size, RootFS: imagetypes.RootFS{Type: "layers", Layers: layers},
		Descriptor: &ocispec.Descriptor{
			MediaType: string(img.Manifest.MediaType), Digest: digest.Digest(img.Manifest.Digest.String()), Size: img.Manifest.Size,
			Platform: &ocispec.Platform{OS: img.Config.OS, Architecture: img.Config.Architecture},
		},
	}, nil
}

func (e *Engine) ImageRemove(ref string, force bool) ([]imagetypes.DeleteResponse, error) {
	r, err := e.images.Remove(ref, force, e.imageInUse)
	switch {
	case errors.Is(err, image.ErrConflict):
		return nil, conflict("%v", err)
	case errors.Is(err, image.ErrNotFound):
		return nil, notFound("No such image: %s", ref)
	case err != nil:
		return nil, err
	}
	out := []imagetypes.DeleteResponse{}
	for _, t := range r.Untagged {
		out = append(out, imagetypes.DeleteResponse{Untagged: t})
	}
	if r.Deleted {
		out = append(out, imagetypes.DeleteResponse{Deleted: r.Image.ID})
	}
	return out, nil
}

func (e *Engine) ImageTag(ref, repo, tag string) error {
	if _, err := e.images.Get(ref); err != nil {
		return notFound("No such image: %s", ref)
	}
	t := repo
	if tag != "" {
		t += ":" + tag
	}
	if err := e.images.Tag(ref, t); err != nil {
		return invalid("%v", err)
	}
	return nil
}

// isDangling reports an image no reference names, as Docker's dangling
// filter does: neither a tag nor a repository@digest.
func isDangling(img image.Image) bool { return len(img.RepoTags) == 0 && len(img.RepoDigests) == 0 }

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// ImagePrune removes the images no container uses that the filters select:
// dangling ones only, unless dangling=false selects every unused image,
// narrowed by until and label filters.
func (e *Engine) ImagePrune(f Filters) (imagetypes.PruneReport, error) {
	danglingOnly := true
	if d, ok := f.boolValue("dangling"); ok {
		danglingOnly = d
	}
	report := imagetypes.PruneReport{ImagesDeleted: []imagetypes.DeleteResponse{}}
	for _, img := range e.images.List() {
		if danglingOnly && !isDangling(img) {
			continue
		}
		if !f.matchLabels(img.Config.Config.Labels) || e.imageInUse(img.ID) {
			continue
		}
		before, err := f.until(img.Created)
		if err != nil {
			return report, err
		}
		if !before {
			continue
		}
		r, err := e.images.Remove(img.ID, true, e.imageInUse)
		if errors.Is(err, image.ErrConflict) || errors.Is(err, image.ErrNotFound) {
			continue
		}
		if err != nil {
			return report, err
		}
		for _, t := range r.Untagged {
			report.ImagesDeleted = append(report.ImagesDeleted, imagetypes.DeleteResponse{Untagged: t})
		}
		report.ImagesDeleted = append(report.ImagesDeleted, imagetypes.DeleteResponse{Deleted: r.Image.ID})
		report.SpaceReclaimed += uint64(r.Image.Size)
	}
	e.events.publish(events.ImageEventType, events.ActionPrune, "", map[string]string{"reclaimed": strconv.FormatUint(report.SpaceReclaimed, 10)})
	return report, nil
}
