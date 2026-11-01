package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	imagetypes "github.com/moby/moby/api/types/image"

	"github.com/e6qu/fun-with-sandboxes/internal/dockerfile"
	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

type CommitOptions struct {
	Container, Repo, Tag string
	Comment, Author      string
	// Changes are Dockerfile instructions applied to the new image's config.
	Changes []string
	Pause   bool
	// Config overrides the container's config in the new image.
	Config *container.Config
}

// commitChanges are the instructions `docker commit --change` accepts.
var commitChanges = map[string]bool{
	"CMD": true, "ENTRYPOINT": true, "ENV": true, "EXPOSE": true, "LABEL": true,
	"USER": true, "VOLUME": true, "WORKDIR": true, "STOPSIGNAL": true,
}

// mountDests lists the container's mount destinations relative to its root:
// the symlinks the engine places there lead to host directories and never
// belong in an image or an export.
func (c *Container) mountDests() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for _, m := range c.rec.Mounts {
		out[strings.TrimPrefix(m.Destination, "/")] = true
	}
	return out
}

// Commit makes an image of the container's root and config, as docker
// commit does: volumes stay out, their mount points left empty.
func (e *Engine) Commit(ctx context.Context, opts CommitOptions) (string, error) {
	c, err := e.Container(opts.Container)
	if err != nil {
		return "", err
	}
	var tags []string
	if opts.Repo != "" {
		ref := opts.Repo
		if opts.Tag != "" {
			ref += ":" + opts.Tag
		}
		n, err := image.Normalize(ref)
		if err != nil {
			return "", invalid("invalid reference format: %v", err)
		}
		tags = append(tags, n)
	}
	base, err := e.images.Get(c.rec.ImageID)
	if err != nil {
		return "", notFound("image of container %s: %v", c.rec.ID, err)
	}

	if opts.Pause && c.isRunning() && !c.isPaused() {
		if err := e.Pause(c.rec.ID); err != nil {
			return "", err
		}
		defer e.Unpause(c.rec.ID)
	}
	if err := os.MkdirAll(e.images.BuildDir(), 0o700); err != nil {
		return "", err
	}
	dst, err := os.MkdirTemp(e.images.BuildDir(), "commit-")
	if err != nil {
		return "", err
	}
	os.Remove(dst)
	if err := image.CloneDir(c.root(), dst); err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dst)
		}
	}()
	for rel := range c.mountDests() {
		p := filepath.Join(dst, rel)
		if err := os.RemoveAll(p); err != nil {
			return "", err
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return "", err
		}
	}

	cfg := *base.Config.DeepCopy()
	c.mu.Lock()
	applyContainerConfig(&cfg.Config, c.rec.Config)
	argv := strings.Join(append(append([]string{}, c.rec.Config.Entrypoint...), c.rec.Config.Cmd...), " ")
	c.mu.Unlock()
	if opts.Config != nil {
		applyContainerConfig(&cfg.Config, *opts.Config)
	}
	if len(opts.Changes) > 0 {
		st := &stage{root: dst, cfg: cfg, args: map[string]string{}, shell: []string{"/bin/sh", "-c"}, workdir: cfg.Config.WorkingDir}
		b := &builder{e: e, progress: func(string) {}}
		for _, ch := range opts.Changes {
			ins, err := dockerfile.Parse(strings.NewReader(ch))
			if err != nil {
				return "", invalid("invalid change %q: %v", ch, err)
			}
			for _, in := range ins {
				if !commitChanges[in.Cmd] {
					return "", invalid("%s is not a valid change command", in.Cmd)
				}
				if err := b.apply(ctx, st, in, nil); err != nil {
					return "", err
				}
			}
		}
		cfg = st.cfg
	}
	now := time.Now().UTC()
	cfg.Created = v1.Time{Time: now}
	cfg.Author = opts.Author
	cfg.History = append(cfg.History, v1.History{Created: cfg.Created, CreatedBy: argv, Comment: opts.Comment, Author: opts.Author})
	img, err := e.images.AddRoot(dst, cfg, tags)
	if err != nil {
		return "", err
	}
	ok = true
	e.publishContainer(c, events.ActionCommit, map[string]string{"comment": opts.Comment, "imageID": img.ID})
	return img.ID, nil
}

// applyContainerConfig carries a container config's set fields into an image
// config, as docker commit records the container's config.
func applyContainerConfig(dst *v1.Config, src container.Config) {
	if len(src.Env) > 0 {
		dst.Env = append([]string(nil), src.Env...)
	}
	if len(src.Cmd) > 0 {
		dst.Cmd = append([]string(nil), src.Cmd...)
	}
	if len(src.Entrypoint) > 0 {
		dst.Entrypoint = append([]string(nil), src.Entrypoint...)
	}
	if src.WorkingDir != "" {
		dst.WorkingDir = src.WorkingDir
	}
	if src.User != "" {
		dst.User = src.User
	}
	if src.StopSignal != "" {
		dst.StopSignal = src.StopSignal
	}
	if len(src.ExposedPorts) > 0 {
		dst.ExposedPorts = map[string]struct{}{}
		for p := range src.ExposedPorts {
			dst.ExposedPorts[p.String()] = struct{}{}
		}
	}
	if len(src.Volumes) > 0 {
		dst.Volumes = map[string]struct{}{}
		for v := range src.Volumes {
			dst.Volumes[v] = struct{}{}
		}
	}
	if len(src.Labels) > 0 {
		if dst.Labels == nil {
			dst.Labels = map[string]string{}
		}
		for k, v := range src.Labels {
			dst.Labels[k] = v
		}
	}
	if h := src.Healthcheck; h != nil && len(h.Test) > 0 {
		dst.Healthcheck = &v1.HealthConfig{Test: h.Test, Interval: h.Interval, Timeout: h.Timeout, StartPeriod: h.StartPeriod, Retries: h.Retries}
	}
}

// Export writes the container's root as a tar, as docker export does:
// volumes stay out, their mount points empty directories.
func (e *Engine) Export(ref string, w io.Writer) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	points := c.mountDests()
	if err := image.WriteTarWith(c.root(), w, func(rel string) bool { return points[rel] }); err != nil {
		return err
	}
	e.publishContainer(c, events.ActionExport, nil)
	return nil
}

// Save writes the named images as a `docker save` archive. An image named by
// ID is saved under all its tags.
func (e *Engine) Save(refs []string, w io.Writer) error {
	images := map[name.Reference]v1.Image{}
	for _, ref := range refs {
		img, rec, err := e.images.V1Image(ref)
		if err != nil {
			return notFound("reference does not exist: %s", ref)
		}
		tags := rec.RepoTags
		if n, err := image.Normalize(ref); err == nil && contains(rec.RepoTags, n) {
			tags = []string{n}
		}
		if len(tags) == 0 {
			return unsupported("image %s has no tags; tag it to save it", ref)
		}
		for _, t := range tags {
			tag, err := name.NewTag(t)
			if err != nil {
				return invalid("%v", err)
			}
			images[tag] = img
		}
	}
	return tarball.MultiRefWrite(images, w)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// PushResult is what a push reports at its end.
type PushResult struct {
	Tag, Digest string
	Size        int64
}

// Push sends a local tag to its registry; with no tag it pushes every tag of
// the repository, as docker push does.
func (e *Engine) Push(ctx context.Context, repo, tag string, auth authn.Authenticator, progress func(string)) ([]PushResult, error) {
	repoRef, err := name.NewRepository(repo)
	if err != nil {
		return nil, invalid("invalid reference format: %v", err)
	}
	want, err := image.Normalize(repo)
	if err != nil {
		return nil, invalid("%v", err)
	}
	wantRepo := want[:strings.LastIndex(want, ":")]
	var tags []string
	for _, img := range e.images.List() {
		for _, t := range img.RepoTags {
			r, tg := t[:strings.LastIndex(t, ":")], t[strings.LastIndex(t, ":")+1:]
			if r == wantRepo && (tag == "" || tg == tag) {
				tags = append(tags, tg)
			}
		}
	}
	if len(tags) == 0 {
		return nil, notFound("tag does not exist: %s", repo)
	}
	progress(fmt.Sprintf("The push refers to repository [%s]", repoRef.Name()))
	var out []PushResult
	for _, tg := range tags {
		img, _, err := e.images.V1Image(wantRepo + ":" + tg)
		if err != nil {
			return out, err
		}
		dst := repoRef.Tag(tg)
		if err := remote.Write(dst, img, remote.WithAuth(auth), remote.WithContext(ctx)); err != nil {
			return out, err
		}
		dg, err := img.Digest()
		if err != nil {
			return out, err
		}
		m, err := img.RawManifest()
		if err != nil {
			return out, err
		}
		progress(fmt.Sprintf("%s: digest: %s size: %d", tg, dg, len(m)))
		out = append(out, PushResult{Tag: tg, Digest: dg.String(), Size: int64(len(m))})
	}
	return out, nil
}

// History lists an image's history, newest first, as docker history does.
func (e *Engine) History(ref string) ([]imagetypes.HistoryResponseItem, error) {
	img, err := e.images.Get(ref)
	if err != nil {
		return nil, notFound("No such image: %s", ref)
	}
	h := img.Config.History
	out := make([]imagetypes.HistoryResponseItem, 0, len(h))
	sizeShown := false
	for i := len(h) - 1; i >= 0; i-- {
		item := imagetypes.HistoryResponseItem{
			ID: "<missing>", Created: h[i].Created.Unix(), CreatedBy: h[i].CreatedBy, Comment: h[i].Comment, Tags: nil,
		}
		if i == len(h)-1 {
			item.ID, item.Tags = img.ID, img.RepoTags
		}
		if !h[i].EmptyLayer && !sizeShown {
			item.Size, sizeShown = img.Size, true
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		out = append(out, imagetypes.HistoryResponseItem{ID: img.ID, Created: img.Created.Unix(), Size: img.Size, Tags: img.RepoTags})
	}
	return out, nil
}
