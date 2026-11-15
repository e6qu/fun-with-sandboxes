package image

import (
	"os"
	"path/filepath"
)

// sweep clears what work an engine did not finish left in the store: roots
// being unpacked, builds' working directories, archives being loaded,
// blobs being downloaded and layers being cached. It runs as the store
// opens, before anything starts such work; each leftover moves to the
// trash at once and goes in the background.
func (s *Store) sweep() {
	trash := filepath.Join(s.dir, "trash")
	var leftovers []string
	for _, pattern := range []string{
		filepath.Join(s.dir, "roots", ".stage-*"),
		filepath.Join(s.BuildDir(), "*"),
		filepath.Join(s.dir, "load-*"),
		filepath.Join(s.dir, "oci", "blobs", "*", ".download-*"),
		filepath.Join(s.layersDir(), ".tmp-*"),
		filepath.Join(trash, "*"),
	} {
		m, _ := filepath.Glob(pattern)
		leftovers = append(leftovers, m...)
	}
	if len(leftovers) == 0 {
		return
	}
	var gone []string
	if err := os.MkdirAll(trash, 0o700); err == nil {
		for _, p := range leftovers {
			if filepath.Dir(p) == trash {
				gone = append(gone, p)
				continue
			}
			t, err := os.MkdirTemp(trash, "")
			if err != nil {
				gone = append(gone, p)
				continue
			}
			if err := os.Rename(p, filepath.Join(t, filepath.Base(p))); err != nil {
				os.Remove(t)
				gone = append(gone, p)
				continue
			}
			gone = append(gone, t)
		}
	} else {
		gone = leftovers
	}
	go func() {
		for _, p := range gone {
			os.RemoveAll(p)
		}
	}()
}
