package engine

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// buildCache is the classic builder's cache: what each step of a build
// produced, under a key that chains every instruction from the base image,
// so a later build with the same chain reuses it, as dockerd's classic
// builder reuses its intermediate images.
type buildCache struct{ dir string }

func (e *Engine) buildCache() *buildCache {
	return &buildCache{dir: filepath.Join(e.cfg.Root, "buildcache")}
}

// cacheEntry is what the cache keeps of one step: when it first ran, which
// its image history records, and the file holding the layer it left, if
// any. A rebuild without the cache writes a new file rather than replace
// one a build in flight may still read.
type cacheEntry struct {
	Created time.Time `json:"created"`
	Layer   string    `json:"layer,omitempty"`
}

func (c *buildCache) entryPath(key string) string { return filepath.Join(c.dir, key+".json") }

func (c *buildCache) layerPath(e cacheEntry) string { return filepath.Join(c.dir, e.Layer) }

// get returns the step under key, when the cache holds it whole.
func (c *buildCache) get(key string) (cacheEntry, bool) {
	b, err := os.ReadFile(c.entryPath(key))
	if err != nil {
		return cacheEntry{}, false
	}
	var e cacheEntry
	if json.Unmarshal(b, &e) != nil {
		return cacheEntry{}, false
	}
	if e.Layer != "" {
		if _, err := os.Stat(c.layerPath(e)); err != nil {
			return cacheEntry{}, false
		}
	}
	return e, true
}

// put records a step. A layer, a file in the same volume, moves into the
// cache first, so an entry never names a layer the cache lacks; put returns
// the entry as stored.
func (c *buildCache) put(key string, created time.Time, layer string) (cacheEntry, error) {
	e := cacheEntry{Created: created}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return e, err
	}
	if layer != "" {
		f, err := os.CreateTemp(c.dir, key+"-*.tar")
		if err != nil {
			return e, err
		}
		f.Close()
		if err := os.Rename(layer, f.Name()); err != nil {
			os.Remove(f.Name())
			return e, err
		}
		e.Layer = filepath.Base(f.Name())
	}
	b, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	tmp, err := os.CreateTemp(c.dir, ".entry-")
	if err != nil {
		return e, err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return e, err
	}
	return e, os.Rename(tmp.Name(), c.entryPath(key))
}

// prune removes every cached step and returns the bytes it freed.
func (c *buildCache) prune() (uint64, error) {
	entries, err := os.ReadDir(c.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var freed uint64
	for _, e := range entries {
		if fi, err := e.Info(); err == nil {
			freed += uint64(fi.Size())
		}
		if err := os.Remove(filepath.Join(c.dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return freed, err
		}
	}
	return freed, nil
}

// cacheKey chains parts into a key.
func cacheKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		io.WriteString(h, p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// contentKey identifies what a layer changes, by each entry's name, type,
// mode, link and content, leaving out times, so the same change made again
// keys the same.
func contentKey(layer string) (string, error) {
	f, err := os.Open(layer)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	tr := tar.NewReader(f)
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%c\x00%o\x00%s\x00%d\x00", hd.Name, hd.Typeflag, hd.Mode, hd.Linkname, hd.Size)
		if _, err := io.Copy(h, tr); err != nil {
			return "", err
		}
	}
}

// argsKey is the build arguments in effect, which a RUN step's environment
// carries and so its cache key does.
func argsKey(args map[string]string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\x00", k, args[k])
	}
	return b.String()
}

// flagsKey is an instruction's flags in a stable order.
func flagsKey(flags map[string]string) string {
	keys := make([]string, 0, len(flags))
	for k := range flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "--%s=%s ", k, flags[k])
	}
	return b.String()
}
