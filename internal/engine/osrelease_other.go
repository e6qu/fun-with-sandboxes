//go:build !darwin

package engine

// hostOSVersion is the host's macOS version; a host that is not macOS,
// where the engine builds and tests portably, has none.
func hostOSVersion() string { return "" }
