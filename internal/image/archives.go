package image

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// A store can stand on a directory of image archives, as `docker save`
// and the catalog's build write them: each archive's names are read when
// the store opens it, and an archive is loaded the first time one of its
// names is asked for and the store holds none. A catalog then costs only
// the images a session uses, and an engine works offline from it.

// archives is the store's directory of archives, by the names they hold.
type archives struct {
	mu     sync.Mutex
	byName map[string]string
	// loads are the archives' loads, each done once: an image removed after
	// its archive loaded stays removed.
	loads map[string]*archiveLoad
}

type archiveLoad struct {
	once sync.Once
	err  error
}

// UseArchives lets the store load the archives in dir, *.tar, when a name
// one of them holds is asked for and the store has no image of that name.
func (s *Store) UseArchives(dir string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.tar"))
	if err != nil {
		return err
	}
	a := &archives{byName: map[string]string{}, loads: map[string]*archiveLoad{}}
	for _, p := range paths {
		names, err := ArchiveNames(p)
		if err != nil {
			return fmt.Errorf("image archive %s: %w", p, err)
		}
		for _, n := range names {
			a.byName[n] = p
		}
	}
	s.mu.Lock()
	s.archives = a
	s.mu.Unlock()
	return nil
}

// fromArchive loads the archive that holds ref, once, and reports whether
// it did so now or before.
func (s *Store) fromArchive(ref string) bool {
	s.mu.Lock()
	a := s.archives
	s.mu.Unlock()
	if a == nil {
		return false
	}
	n, err := Normalize(ref)
	if err != nil {
		return false
	}
	a.mu.Lock()
	path, ok := a.byName[n]
	var l *archiveLoad
	if ok {
		if l = a.loads[path]; l == nil {
			l = &archiveLoad{}
			a.loads[path] = l
		}
	}
	a.mu.Unlock()
	if !ok {
		return false
	}
	l.once.Do(func() { _, l.err = s.loadPath(path) })
	return l.err == nil
}

// ArchiveNames lists the normalized names the archive at path gives its
// images, from its manifest.json or OCI index.json, without unpacking it.
func ArchiveNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		switch filepath.Clean(h.Name) {
		case "manifest.json":
			var m []manifestEntry
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return nil, fmt.Errorf("manifest.json: %w", err)
			}
			for _, e := range m {
				for _, t := range e.RepoTags {
					if n, err := Normalize(t); err == nil {
						names = append(names, n)
					}
				}
			}
		case "index.json":
			var idx struct {
				Manifests []struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"manifests"`
			}
			if err := json.NewDecoder(tr).Decode(&idx); err != nil {
				return nil, fmt.Errorf("index.json: %w", err)
			}
			for _, d := range idx.Manifests {
				if t := annotatedName(d.Annotations); t != "" {
					if n, err := Normalize(t); err == nil && !strings.Contains(n, "@") {
						names = append(names, n)
					}
				}
			}
		}
	}
}
