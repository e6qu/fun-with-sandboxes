package engine

import "golang.org/x/sys/unix"

// pressureLevel is macOS's memory pressure level.
func pressureLevel() (int, error) {
	l, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	return int(l), err
}

// diskFree is the space an unprivileged process may still write on the
// volume that holds path.
func diskFree(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
