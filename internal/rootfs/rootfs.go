// Package rootfs resolves paths inside a root directory the way a chroot
// would, so code outside any sandbox never follows a path out of the root.
package rootfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// maxSymlinks bounds symlink resolution, as the kernel's ELOOP does.
const maxSymlinks = 40

// Join maps a path inside a root directory to the host, resolving each
// existing symlink as a chroot would: absolute targets from the root,
// relative ones from their directory, never above the root. Components that
// do not exist yet are kept as given, so the result is safe to create.
func Join(root, p string) (string, error) {
	// ".." is resolved during the walk, after the symlinks before it, as the
	// kernel resolves it; cleaning the path first would not.
	rest := strings.Split(p, "/")
	var done []string
	hops := 0
	for len(rest) > 0 {
		part := rest[0]
		rest = rest[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(done) > 0 {
				done = done[:len(done)-1]
			}
			continue
		}
		cur := filepath.Join(append([]string{root}, append(done, part)...)...)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			done = append(done, part)
			done = append(done, rest...)
			break
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			done = append(done, part)
			continue
		}
		if hops++; hops > maxSymlinks {
			return "", errors.New("too many levels of symbolic links in " + p)
		}
		target, err := os.Readlink(cur)
		if err != nil {
			return "", err
		}
		// An absolute target starts again from the root; a relative one from
		// the symlink's directory, which done already holds.
		if strings.HasPrefix(target, "/") {
			done = nil
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	clean := []string{}
	for _, d := range done {
		switch d {
		case "", ".":
		case "..":
			if len(clean) > 0 {
				clean = clean[:len(clean)-1]
			}
		default:
			clean = append(clean, d)
		}
	}
	return filepath.Join(append([]string{root}, clean...)...), nil
}
