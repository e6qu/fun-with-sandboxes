package image

import "golang.org/x/sys/unix"

// Clone gives dst an APFS copy-on-write clone of the image root.
func (s *Store) Clone(id, dst string) error {
	return unix.Clonefile(s.Root(id), dst, unix.CLONE_NOFOLLOW)
}
