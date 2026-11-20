package engine

import "golang.org/x/sys/unix"

// hostOSVersion is the host's macOS version, as sw_vers prints it.
func hostOSVersion() string {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return v
}
