//go:build !darwin

package main

import "errors"

var errNoShm = errors.New("shm: macOS only")

// shm fails: the toolbox calls libSystem's shm_open, which only macOS has.
func shm(name string) error { return errNoShm }

func shmMake(name string) error { return errNoShm }

func shmOpen(name string) error { return errNoShm }
