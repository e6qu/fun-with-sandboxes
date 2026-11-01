package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/volume"
)

type Volume struct {
	Name      string            `json:"name"`
	CreatedAt time.Time         `json:"createdAt"`
	Labels    map[string]string `json:"labels"`
	Options   map[string]string `json:"options"`
	Anonymous bool              `json:"anonymous"`
	// Fresh is set until the first mount copies the image content in.
	Fresh bool `json:"fresh"`
}

var validVolumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func (e *Engine) volumesPath() string { return filepath.Join(e.cfg.Root, "volumes.json") }

// VolumeDir is the host directory holding the volume's data.
func (e *Engine) VolumeDir(name string) string {
	return filepath.Join(e.cfg.Root, "volumes", name, "_data")
}

func (e *Engine) loadVolumes() error {
	_, err := readJSON(e.volumesPath(), &e.volumes)
	if e.volumes == nil {
		e.volumes = map[string]*Volume{}
	}
	return err
}

func (e *Engine) saveVolumesLocked() error { return writeJSON(e.volumesPath(), e.volumes) }

func (e *Engine) VolumeCreate(req volume.CreateRequest) (volume.Volume, error) {
	if req.Driver != "" && req.Driver != "local" {
		return volume.Volume{}, unsupported("volume driver %q is not supported; only \"local\" is", req.Driver)
	}
	if len(req.DriverOpts) > 0 {
		return volume.Volume{}, unsupported("local volume options are not supported")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	name := req.Name
	anonymous := name == ""
	if anonymous {
		name = newID()
	}
	if v, ok := e.volumes[name]; ok {
		return e.volumeView(v), nil
	}
	v, err := e.createVolumeLocked(name, req.Labels, anonymous)
	if err != nil {
		return volume.Volume{}, err
	}
	return e.volumeView(v), nil
}

func (e *Engine) createVolumeLocked(name string, labels map[string]string, anonymous bool) (*Volume, error) {
	if !validVolumeName.MatchString(name) {
		return nil, invalid("create %s: %q includes invalid characters for a local volume name", name, name)
	}
	if err := os.MkdirAll(e.VolumeDir(name), 0o755); err != nil {
		return nil, err
	}
	v := &Volume{Name: name, CreatedAt: time.Now().UTC(), Labels: labels, Anonymous: anonymous, Fresh: true}
	e.volumes[name] = v
	if err := e.saveVolumesLocked(); err != nil {
		delete(e.volumes, name)
		return nil, err
	}
	e.events.publish(events.VolumeEventType, events.ActionCreate, name, map[string]string{"driver": "local"})
	return v, nil
}

func (e *Engine) volumeView(v *Volume) volume.Volume {
	labels := v.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return volume.Volume{
		Name:       v.Name,
		Driver:     "local",
		Mountpoint: e.VolumeDir(v.Name),
		CreatedAt:  v.CreatedAt.Format(time.RFC3339),
		Labels:     labels,
		Options:    map[string]string{},
		Scope:      "local",
	}
}

func (e *Engine) VolumeInspect(name string) (volume.Volume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.volumes[name]
	if !ok {
		return volume.Volume{}, notFound("get %s: no such volume", name)
	}
	return e.volumeView(v), nil
}

func (e *Engine) VolumeList(f Filters) []volume.Volume {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []volume.Volume
	for _, v := range e.volumes {
		if !f.matchName("name", v.Name) || !f.matchLabels(v.Labels) {
			continue
		}
		if dangling, ok := f.boolValue("dangling"); ok && dangling == e.volumeInUseLocked(v.Name) {
			continue
		}
		out = append(out, e.volumeView(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (e *Engine) volumeInUseLocked(name string) bool {
	for _, c := range e.containers {
		for _, m := range c.rec.Mounts {
			if m.Type == "volume" && m.Name == name {
				return true
			}
		}
	}
	return false
}

func (e *Engine) VolumeRemove(name string, force bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.volumes[name]; !ok {
		if force {
			return nil
		}
		return notFound("get %s: no such volume", name)
	}
	if e.volumeInUseLocked(name) {
		return conflict("remove %s: volume is in use", name)
	}
	return e.removeVolumeLocked(name)
}

func (e *Engine) removeVolumeLocked(name string) error {
	if err := os.RemoveAll(filepath.Dir(e.VolumeDir(name))); err != nil {
		return err
	}
	delete(e.volumes, name)
	e.events.publish(events.VolumeEventType, events.ActionDestroy, name, map[string]string{"driver": "local"})
	return e.saveVolumesLocked()
}

// VolumePrune removes unused volumes; without all it keeps named ones, as
// Docker does since API 1.42.
func (e *Engine) VolumePrune(f Filters) (volume.PruneReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	all, _ := f.boolValue("all")
	var rep volume.PruneReport
	for name, v := range e.volumes {
		if e.volumeInUseLocked(name) || (!all && !v.Anonymous) || !f.matchLabels(v.Labels) {
			continue
		}
		size := dirSize(e.VolumeDir(name))
		if err := e.removeVolumeLocked(name); err != nil {
			return rep, err
		}
		rep.VolumesDeleted = append(rep.VolumesDeleted, name)
		rep.SpaceReclaimed += uint64(size)
	}
	return rep, nil
}

func dirSize(dir string) int64 {
	var n int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}
