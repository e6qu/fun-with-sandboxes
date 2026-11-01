//go:build !darwin

package image

import "errors"

// Clone needs APFS clonefile(2), which only macOS provides.
func (s *Store) Clone(id, dst string) error {
	return errors.New("image: cloning a root requires macOS (APFS clonefile)")
}
