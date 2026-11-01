package config

import "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/compression"

type RefConfig struct {
	Compression            compression.Config
	PreferNonDistributable bool
}
