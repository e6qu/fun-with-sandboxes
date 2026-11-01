package resources

import resourcestypes "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/executor/resources/types"

type SysSampler = Sub[*resourcestypes.SysSample]

func NewSysSampler() (*Sampler[*resourcestypes.SysSample], error) {
	return newSysSampler()
}
