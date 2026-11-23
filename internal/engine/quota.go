package engine

import (
	"io/fs"
	"path/filepath"
	"strings"

	units "github.com/docker/go-units"
)

// minQuota is the smallest root size quota, which an APFS volume of a disk
// image needs room for.
const minQuota = 64 << 20

// storageQuota reads a container's storage options: size, the most its
// root's files may grow by beyond its image's, as overlay2's size quota
// bounds the writable layer. It is 0 without one.
func storageQuota(opts map[string]string) (int64, error) {
	var size int64
	for k, v := range opts {
		if strings.ToLower(k) != "size" {
			return 0, unsupported("storage option %s is not supported: the root's size alone is", k)
		}
		n, err := units.RAMInBytes(v)
		if err != nil || n <= 0 {
			return 0, invalid("invalid storage option size %q", v)
		}
		if n < minQuota {
			return 0, invalid("storage option size %s is below the smallest, 64MiB", v)
		}
		size = n
	}
	return size, nil
}

// treeBytes is the size of the files under dir.
func treeBytes(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			n += info.Size()
		}
		return nil
	})
	return n, err
}

// quotaImage is where a container with a size quota keeps its root's disk
// image.
func quotaImage(dir string) string { return filepath.Join(dir, "root.sparseimage") }
