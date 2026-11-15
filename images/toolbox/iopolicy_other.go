//go:build !darwin

package main

import "errors"

// ioPolicy fails: disk I/O policy tiers are macOS's.
func ioPolicy(bool) error { return errors.New("iopolicy: macOS only") }
