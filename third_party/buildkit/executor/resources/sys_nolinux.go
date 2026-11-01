//go:build !linux

package resources

import resourcestypes "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/executor/resources/types"

func newSysSampler() (*Sampler[*resourcestypes.SysSample], error) {
	return nil, nil
}
