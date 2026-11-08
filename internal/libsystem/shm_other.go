//go:build !darwin

package libsystem

import "errors"

// ShmUnlink fails: the engine's shared memory objects are macOS's.
func ShmUnlink(name string) error { return errors.New("shm_unlink: macOS only") }
