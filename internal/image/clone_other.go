//go:build !darwin

package image

import "errors"

var errNoClonefile = errors.New("image: cloning a root requires macOS (APFS clonefile)")

// Clone needs APFS clonefile(2), which only macOS provides.
func (s *Store) Clone(id, dst string) error { return errNoClonefile }

func CloneDir(src, dst string) error { return errNoClonefile }
