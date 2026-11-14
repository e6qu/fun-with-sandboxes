//go:build !darwin

package engine

import "errors"

// pressureLevel has no counterpart off macOS.
func pressureLevel() (int, error) { return 0, errors.New("no memory pressure level") }
