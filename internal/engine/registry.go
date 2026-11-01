package engine

import (
	"bufio"
	"compress/bzip2"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/moby/moby/api/types/events"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// Login checks credentials against a registry, as dockerd does for docker
// login: it pings the registry's API with them, through a token exchange
// when the registry asks for one.
func (e *Engine) Login(ctx context.Context, server string, cfg authn.AuthConfig) error {
	host := image.RegistryHost(server)
	reg, err := name.NewRegistry(host)
	if err != nil {
		return invalid("invalid registry %q: %v", server, err)
	}
	auth := image.Authenticator(cfg)
	tr, err := transport.NewWithContext(ctx, reg, auth, http.DefaultTransport, nil)
	if err != nil {
		var te *transport.Error
		if errors.As(err, &te) && te.StatusCode == http.StatusUnauthorized {
			return unauthorized("login attempt to %s://%s/v2/ failed with status: 401 Unauthorized", reg.Scheme(), host)
		}
		return fmt.Errorf("login to %s: %w", host, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.Scheme()+"://"+host+"/v2/", nil)
	if err != nil {
		return err
	}
	res, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return fmt.Errorf("login to %s: %w", host, err)
	}
	res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return unauthorized("login attempt to %s://%s/v2/ failed with status: %s", reg.Scheme(), host, res.Status)
	case res.StatusCode >= 300:
		return fmt.Errorf("login attempt to %s://%s/v2/ failed with status: %s", reg.Scheme(), host, res.Status)
	}
	return nil
}

// PushResult is what a push reports at its end.
type PushResult struct {
	Tag, Digest string
	Size        int64
}

// Push sends a local tag to its registry; with no tag it pushes every tag of
// the repository, as docker push does. Each layer reports as dockerd's do:
// Preparing, then Layer already exists, or Pushing and Pushed.
func (e *Engine) Push(ctx context.Context, repo, tag string, auth authn.Authenticator, progress image.Progress) ([]PushResult, error) {
	repoRef, err := name.NewRepository(repo)
	if err != nil {
		return nil, invalid("invalid reference format: %v", err)
	}
	wantRepo := image.Familiar(repoRef)
	var tags []string
	for _, img := range e.images.List() {
		for _, t := range img.RepoTags {
			if image.Repository(t) == wantRepo && (tag == "" || strings.TrimPrefix(t, wantRepo+":") == tag) {
				tags = append(tags, strings.TrimPrefix(t, wantRepo+":"))
			}
		}
	}
	if len(tags) == 0 {
		return nil, notFound("tag does not exist: %s", repo)
	}
	tr, err := transport.NewWithContext(ctx, repoRef.Registry, auth, http.DefaultTransport, []string{repoRef.Scope(transport.PushScope)})
	if err != nil {
		var te *transport.Error
		if errors.As(err, &te) && te.StatusCode == http.StatusUnauthorized {
			return nil, unauthorized("authentication required to push to %s", repoRef.Name())
		}
		return nil, err
	}
	client := &http.Client{Transport: tr}
	opts := []remote.Option{remote.WithAuth(auth), remote.WithContext(ctx)}
	progress("", fmt.Sprintf("The push refers to repository [%s]", repoRef.Name()))
	var out []PushResult
	for _, tg := range tags {
		img, rec, err := e.images.V1Image(wantRepo + ":" + tg)
		if err != nil {
			return out, err
		}
		layers, err := img.Layers()
		if err != nil {
			return out, err
		}
		for _, l := range layers {
			if err := pushLayer(ctx, client, repoRef, l, opts, progress); err != nil {
				return out, err
			}
		}
		if err := remote.Write(repoRef.Tag(tg), img, opts...); err != nil {
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
		if err := e.images.AddReference(rec.ID, wantRepo+"@"+dg.String()); err != nil {
			return out, err
		}
		progress("", fmt.Sprintf("%s: digest: %s size: %d", tg, dg, len(m)))
		out = append(out, PushResult{Tag: tg, Digest: dg.String(), Size: int64(len(m))})
	}
	return out, nil
}

// pushLayer uploads one layer unless the repository holds it already.
func pushLayer(ctx context.Context, client *http.Client, repo name.Repository, l v1.Layer, opts []remote.Option, progress image.Progress) error {
	dg, err := l.Digest()
	if err != nil {
		return err
	}
	id := dg.Hex[:12]
	progress(id, "Preparing")
	url := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", repo.Scheme(), repo.RegistryStr(), repo.RepositoryStr(), dg)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode == http.StatusOK {
		progress(id, "Layer already exists")
		return nil
	}
	progress(id, "Pushing")
	if err := remote.WriteLayer(repo, l, opts...); err != nil {
		return err
	}
	progress(id, "Pushed")
	return nil
}

// ImportOptions describe a `docker import`.
type ImportOptions struct {
	// Source is "-" for the request body, or an http(s) URL.
	Source  string
	Repo    string
	Tag     string
	Message string
	Changes []string
}

// Import makes a one-layer image of a filesystem tarball, plain or
// compressed with gzip, bzip2 or zstd, as docker import does.
func (e *Engine) Import(ctx context.Context, opts ImportOptions, body io.Reader) (string, error) {
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
	src := body
	if opts.Source != "-" {
		if !strings.HasPrefix(opts.Source, "http://") && !strings.HasPrefix(opts.Source, "https://") {
			return "", invalid("import source %q is neither - nor an http(s) URL", opts.Source)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.Source, nil)
		if err != nil {
			return "", invalid("%v", err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return "", fmt.Errorf("fetch %s: %s", opts.Source, res.Status)
		}
		src = res.Body
	}
	if err := os.MkdirAll(e.images.BuildDir(), 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(e.images.BuildDir(), "import-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	layer := filepath.Join(dir, "layer.tar")
	if err := decompressTo(src, layer); err != nil {
		return "", invalid("read the imported archive: %v", err)
	}
	root := filepath.Join(dir, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		return "", err
	}
	f, err := os.Open(layer)
	if err != nil {
		return "", err
	}
	err = image.ApplyLayer(root, f)
	f.Close()
	if err != nil {
		return "", invalid("unpack the imported archive: %v", err)
	}

	now := v1.Time{Time: time.Now().UTC()}
	comment := opts.Message
	if comment == "" {
		comment = "Imported from " + opts.Source
	}
	cfg := v1.ConfigFile{Created: now, History: []v1.History{{Created: now, Comment: comment}}}
	if cfg, err = e.applyChanges(ctx, cfg, opts.Changes); err != nil {
		return "", err
	}
	img, err := e.images.Extend("", []string{layer}, root, cfg, tags)
	if err != nil {
		return "", err
	}
	e.events.publish(events.ImageEventType, events.ActionImport, img.ID, nil)
	return img.ID, nil
}

// decompressTo writes the tar r holds, decompressed, to path.
func decompressTo(r io.Reader, path string) error {
	br := bufio.NewReader(r)
	var plain io.Reader
	// containerd's detector knows gzip and zstd; Docker also takes bzip2.
	if magic, _ := br.Peek(3); string(magic) == "BZh" {
		plain = bzip2.NewReader(br)
	} else {
		ds, err := compression.DecompressStream(br)
		if err != nil {
			return err
		}
		defer ds.Close()
		plain = ds
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, plain); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
