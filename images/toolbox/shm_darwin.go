package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
)

// shm creates and removes the POSIX shared memory object name.
func shm(name string) error {
	if err := shmMake(name); err != nil {
		return err
	}
	return libsystem.ShmUnlink(name)
}

// shmMake creates the POSIX shared memory object name and leaves it, as
// the objects outlive their processes until removed.
func shmMake(name string) error {
	fd, err := libsystem.ShmOpen(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("shm_open %s: %w", name, err)
	}
	return syscall.Close(fd)
}

// shmOpen opens the existing POSIX shared memory object name and removes it.
func shmOpen(name string) error {
	fd, err := libsystem.ShmOpen(name, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("shm_open %s: %w", name, err)
	}
	syscall.Close(fd)
	return libsystem.ShmUnlink(name)
}
