package builder

import (
	"context"
	"fmt"

	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/client/llb/sourceresolver"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend/gateway/client"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/flightcontrol"
	"github.com/gohugoio/hashstructure"
	digest "github.com/opencontainers/go-digest"
)

// withResolveCache wraps client.Client so that ResolveImageConfig
// calls are cached and deduplicated via flightcontrol.CachedGroup
type withResolveCache struct {
	client.Client
	g flightcontrol.CachedGroup[*resolveResult]
}

type resolveResult struct {
	ref  string
	dgst digest.Digest
	cfg  []byte
}

var _ client.Client = &withResolveCache{}

// newWithResolveCache sets CacheError once, before any call: setting it in
// every ResolveImageConfig raced, as stages resolve their bases concurrently.
func newWithResolveCache(c client.Client) *withResolveCache {
	w := &withResolveCache{Client: c}
	w.g.CacheError = true
	return w
}

func (c *withResolveCache) ResolveImageConfig(ctx context.Context, ref string, opt sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	optHash, err := hashstructure.Hash(opt, nil)
	if err != nil {
		return "", "", nil, err
	}
	key := fmt.Sprintf("%s,%d", ref, optHash)
	res, err := c.g.Do(ctx, key, func(ctx context.Context) (*resolveResult, error) {
		ref, dgst, cfg, err := c.Client.ResolveImageConfig(ctx, ref, opt)
		if err != nil {
			return nil, err
		}
		return &resolveResult{ref, dgst, cfg}, nil
	})
	if err != nil {
		return "", "", nil, err
	}
	return res.ref, res.dgst, res.cfg, nil
}
