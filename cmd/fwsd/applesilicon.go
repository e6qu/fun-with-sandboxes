//go:build darwin && !arm64

package main

// fwsd runs on Apple silicon alone: a darwin build for another
// architecture stops here.
var _ = fwsdRunsOnAppleSiliconAlone
