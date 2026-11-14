package engine

import "golang.org/x/sys/unix"

// pressureLevel is macOS's memory pressure level.
func pressureLevel() (int, error) {
	l, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	return int(l), err
}
