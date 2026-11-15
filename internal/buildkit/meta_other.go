//go:build !darwin

package buildkit

import (
	"os"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// cloneTree needs APFS clonefile(2), which only macOS provides.
func cloneTree(src, dst string) error { return image.CloneDir(src, dst) }

// copyDirMeta gives the directory dst the mode of src.
func copyDirMeta(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode().Perm())
}
