//go:build !darwin

package engine

import "errors"

// cloneInto needs APFS clonefile(2), which only macOS provides; a volume's
// copy-up copies instead.
func cloneInto(src, dst string) error { return errors.New("cloning needs APFS") }
