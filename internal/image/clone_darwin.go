package image

import "golang.org/x/sys/unix"

// Clone gives dst an APFS copy-on-write clone of the image root.
func (s *Store) Clone(id, dst string) error { return CloneDir(s.Root(id), dst) }

// CloneDir clones the tree at src to dst, which must not exist.
func CloneDir(src, dst string) error { return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) }
