package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
)

// shm creates and removes the POSIX shared memory object name.
func shm(name string) error {
	fd, err := libsystem.ShmOpen(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("shm_open %s: %w", name, err)
	}
	syscall.Close(fd)
	return libsystem.ShmUnlink(name)
}
