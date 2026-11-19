package cri

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/e6qu/fun-with-sandboxes/internal/containerd"
	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// A kind node's containerd configuration, which kind's
// containerdConfigPatches change, names the registries an image is pulled
// through (package containerd). The runtime reads it from the node at each
// pull, as containerd reads its hosts directory, and reports any other
// setting a patch made as one it does not apply.

// nodeSettings are what the node's containerd configuration asks of the
// runtime; a runtime on no node, or a node without the file, asks nothing.
func (s *Service) nodeSettings() (containerd.Settings, error) {
	b, err := s.e.NodeFile(containerd.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return containerd.Settings{}, nil
	}
	if err != nil {
		return containerd.Settings{}, err
	}
	return containerd.Parse(b)
}

// hostKey is the name containerd knows ref's registry by: Docker Hub's is
// docker.io.
func hostKey(ref string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	if h := r.Context().RegistryStr(); h != name.DefaultRegistry {
		return h, nil
	}
	return "docker.io", nil
}

// endpoints are the registries ref is pulled through before its own: its
// registry's mirrors, then those its hosts.toml names.
func (s *Service) endpoints(ref string) ([]containerd.Endpoint, error) {
	set, err := s.nodeSettings()
	if err != nil {
		return nil, err
	}
	host, err := hostKey(ref)
	if err != nil {
		return nil, err
	}
	out := append([]containerd.Endpoint(nil), set.Mirrors[host]...)
	if set.HostsDir != "" {
		hosts, err := containerd.Hosts(s.e.NodeFile, set.HostsDir, host)
		if err != nil {
			return nil, err
		}
		out = append(out, hosts...)
	}
	return out, nil
}

// pull pulls ref through each of its endpoints in turn, reached as the
// node reaches them, then from its registry, and says why each failed
// where all do.
func (s *Service) pull(ctx context.Context, ref string, auth authn.Authenticator) (image.Pulled, error) {
	eps, err := s.endpoints(ref)
	if err != nil {
		return image.Pulled{}, err
	}
	var failed []string
	for _, ep := range eps {
		p, err := s.e.Images().PullVia(ctx, ref, auth, func(string, string) {},
			image.Via{Registry: ep.Host, Insecure: ep.Insecure, SkipVerify: ep.SkipVerify, Dial: s.e.DialNode})
		if err == nil {
			return p, nil
		}
		failed = append(failed, fmt.Sprintf("%s: %v", ep.Host, err))
	}
	p, err := s.e.Images().Pull(ctx, ref, auth, func(string, string) {})
	if err != nil && len(failed) > 0 {
		return p, fmt.Errorf("%w; through %s", err, strings.Join(failed, "; through "))
	}
	return p, err
}

// unappliedCondition is the runtime's condition while the node's
// configuration sets what the runtime does not apply, or cannot be read.
func (s *Service) unappliedCondition() (reason, message string, ok bool) {
	set, err := s.nodeSettings()
	switch {
	case err != nil:
		return "ContainerdConfigUnreadable", err.Error(), true
	case len(set.Unsupported) > 0:
		return "ContainerdConfigUnsupported", fmt.Sprintf("%s sets %s, which this node's runtime, the engine fwsd nests in it, does not apply: it takes the registries an image is pulled through alone",
			containerd.ConfigPath, strings.Join(set.Unsupported, ", ")), true
	}
	return "", "", false
}
