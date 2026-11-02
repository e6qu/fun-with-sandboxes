//go:build !darwin

package main

import "errors"

// shm fails: the toolbox calls libSystem's shm_open, which only macOS has.
func shm(name string) error {
	return errors.New("shm: macOS only")
}
