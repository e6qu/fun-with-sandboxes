// Package image keeps the engine's images. Each one is stored as an OCI
// image (manifest, config and layers) in an OCI image layout, which save,
// push and BuildKit read, and unpacked as a root directory that containers
// receive as an APFS clone.
package image

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/gzip"
)

var ErrNotFound = errors.New("image not found")

// Platform is the only platform the engine runs.
var Platform = v1.Platform{OS: "darwin", Architecture: runtime.GOARCH}

// Image is a stored image. Its ID is its config's digest, as in Docker.
type Image struct {
	ID       string   `json:"id"`
	RepoTags []string `json:"repoTags"`
	// RepoDigests are repository@digest references: the digest a pull
	// resolved, or a push wrote, in that repository.
	RepoDigests []string  `json:"repoDigests"`
	Created     time.Time `json:"created"`
	// Size is the sum of LayerSizes.
	Size   int64         `json:"size"`
	Config v1.ConfigFile `json:"config"`
	// HealthStartInterval is the health check's start interval, which
	// Config has no field for.
	HealthStartInterval time.Duration `json:"healthStartInterval,omitempty"`
	// Manifest is the image manifest's descriptor in the store's layout.
	Manifest v1.Descriptor `json:"manifest"`
	// LayerSizes are the uncompressed sizes of the manifest's layers.
	LayerSizes []int64 `json:"layerSizes"`
	// RootUsage is what the image's unpacked root takes on disk, which the
	// store measures once, as it unpacks it.
	RootUsage *Usage `json:"rootUsage,omitempty"`
}

// Usage is what files take: their bytes on disk and their inodes.
type Usage struct {
	Bytes  uint64 `json:"bytes"`
	Inodes uint64 `json:"inodes"`
}

// Add adds v to u.
func (u *Usage) Add(v Usage) {
	u.Bytes += v.Bytes
	u.Inodes += v.Inodes
}

// DiskUsage is what the files under dir take, dir included.
func DiskUsage(dir string) Usage {
	var u Usage
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		u.Inodes++
		if info, err := d.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				u.Bytes += uint64(st.Blocks) * 512
			}
		}
		return nil
	})
	return u
}

type Store struct {
	dir string
	mu  sync.Mutex
	idx map[string]*Image
	// blobs is held shared while an image's blobs are written and
	// exclusively while unreferenced blobs are collected.
	blobs sync.RWMutex
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "roots"), 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, idx: map[string]*Image{}}
	b, err := os.ReadFile(s.indexPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.idx); err != nil {
			return nil, fmt.Errorf("image: read index: %w", err)
		}
	}
	for id, img := range s.idx {
		if img.Manifest.Digest.Hex == "" {
			return nil, fmt.Errorf("image: %s holds image %s in the flattened format an earlier engine wrote; remove the directory to start a layered store", dir, id)
		}
	}
	if err := s.openLayout(); err != nil {
		return nil, fmt.Errorf("image: open the OCI layout: %w", err)
	}
	// An index an earlier engine wrote lacks the start interval.
	for id, img := range s.idx {
		if img.HealthStartInterval != 0 || img.Config.Config.Healthcheck == nil {
			continue
		}
		v, _, err := s.V1Image(id)
		if err != nil {
			return nil, err
		}
		raw, err := v.RawConfigFile()
		if err != nil {
			return nil, err
		}
		img.HealthStartInterval = healthStartInterval(raw)
	}
	return s, nil
}

func (s *Store) indexPath() string { return filepath.Join(s.dir, "images.json") }

// Root returns the image's read-only reference root directory.
func (s *Store) Root(id string) string {
	return filepath.Join(s.dir, "roots", strings.TrimPrefix(id, "sha256:"))
}

// Normalize turns a user reference into the familiar form Docker shows in
// RepoTags and RepoDigests, e.g. "postgres" -> "postgres:latest" and
// "docker.io/library/postgres@sha256:…" -> "postgres@sha256:…".
func Normalize(ref string) (string, error) {
	r, err := name.ParseReference(ref, name.WithDefaultTag("latest"))
	if err != nil {
		return "", err
	}
	switch r := r.(type) {
	case name.Tag:
		return Familiar(r.Context()) + ":" + r.TagStr(), nil
	case name.Digest:
		return Familiar(r.Context()) + "@" + r.DigestStr(), nil
	}
	return r.Name(), nil
}

// Familiar names a repository as Docker shows it: Docker Hub's registry and
// its library/ namespace left out.
func Familiar(repo name.Repository) string {
	if repo.RegistryStr() == name.DefaultRegistry {
		return strings.TrimPrefix(repo.RepositoryStr(), "library/")
	}
	return repo.RegistryStr() + "/" + repo.RepositoryStr()
}

// isDigestRef reports whether a normalized reference is repository@digest.
func isDigestRef(ref string) bool { return strings.Contains(ref, "@") }

// Repository is a normalized reference's repository.
func Repository(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

func (img *Image) refs() []string {
	return append(append([]string(nil), img.RepoTags...), img.RepoDigests...)
}

// Get resolves a reference, a full ID, or an unambiguous ID prefix.
func (s *Store) Get(ref string) (Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	img, err := s.getLocked(ref)
	if err != nil {
		return Image{}, err
	}
	return *img, nil
}

func (s *Store) getLocked(ref string) (*Image, error) {
	if img, ok := s.idx[ref]; ok {
		return img, nil
	}
	if n, err := Normalize(ref); err == nil {
		for _, img := range s.idx {
			for _, t := range img.refs() {
				if t == n {
					return img, nil
				}
			}
		}
	}
	hex := strings.TrimPrefix(ref, "sha256:")
	if len(hex) >= 4 && isHex(hex) {
		var match *Image
		for id, img := range s.idx {
			if strings.HasPrefix(strings.TrimPrefix(id, "sha256:"), hex) {
				if match != nil {
					return nil, fmt.Errorf("image reference %q is ambiguous", ref)
				}
				match = img
			}
		}
		if match != nil {
			return match, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
}

func (s *Store) List() []Image {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Image, 0, len(s.idx))
	for _, img := range s.idx {
		out = append(out, *img)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Tag adds a tag to an image, moving it off any image that held it.
func (s *Store) Tag(ref, tag string) error {
	n, err := Normalize(tag)
	if err != nil {
		return err
	}
	if isDigestRef(n) {
		return fmt.Errorf("refusing to create a tag with a digest reference")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	img, err := s.getLocked(ref)
	if err != nil {
		return err
	}
	s.addRefsLocked(img, []string{n})
	return s.saveLocked()
}

// addRefsLocked points normalized references, tags or digests, at img,
// moving each off any image that held it.
func (s *Store) addRefsLocked(img *Image, refs []string) {
	for _, n := range refs {
		s.untagLocked(n)
		if isDigestRef(n) {
			img.RepoDigests = append(img.RepoDigests, n)
		} else {
			img.RepoTags = append(img.RepoTags, n)
		}
	}
}

// AddReference points a normalized repository@digest reference at the image
// id, as a push records the digest it wrote.
func (s *Store) AddReference(id, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	img, ok := s.idx[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	s.addRefsLocked(img, []string{ref})
	return s.saveLocked()
}

func (s *Store) untagLocked(n string) {
	for _, other := range s.idx {
		other.RepoTags = without(other.RepoTags, n)
		other.RepoDigests = without(other.RepoDigests, n)
	}
}

// without is list without v. It builds a new slice, as the records' slices
// are shared with the copies the store has handed out.
func without(list []string, v string) []string {
	if !slices.Contains(list, v) {
		return list
	}
	kept := make([]string, 0, len(list)-1)
	for _, t := range list {
		if t != v {
			kept = append(kept, t)
		}
	}
	return kept
}

// Remove deletes the image. inUse reports whether a container still needs it.
// ErrConflict reports a removal Docker refuses without force.
var ErrConflict = errors.New("conflict")

// Removal says what Remove did: the tags it dropped, and whether it deleted
// the image itself.
type Removal struct {
	Image    Image
	Untagged []string
	Deleted  bool
}

// Remove follows `docker rmi`: naming a reference (a tag or a
// repository@digest) of an image that has others drops only that reference,
// and the repository's digest references with its last tag; naming the last
// reference or the ID deletes the image, which no container may still use,
// and an ID referenced from several repositories needs force.
func (s *Store) Remove(ref string, force bool, inUse func(id string) bool) (Removal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	img, err := s.getLocked(ref)
	if err != nil {
		return Removal{}, err
	}
	var untagged []string
	named := ""
	if n, err := Normalize(ref); err == nil {
		for _, t := range img.refs() {
			if t == n {
				named = n
			}
		}
	}
	if named != "" && len(img.refs()) > 1 {
		untagged = append(untagged, named)
		s.untagLocked(named)
		if !isDigestRef(named) && !hasTagIn(img.RepoTags, Repository(named)) {
			for _, d := range append([]string(nil), img.RepoDigests...) {
				if Repository(d) == Repository(named) {
					s.untagLocked(d)
					untagged = append(untagged, d)
				}
			}
		}
		if len(img.refs()) > 0 {
			return Removal{Image: *img, Untagged: untagged}, s.saveLocked()
		}
	}
	if named == "" && len(repos(img.refs())) > 1 && !force {
		return Removal{}, fmt.Errorf("%w: unable to delete %s (must be forced) - image is referenced in multiple repositories", ErrConflict, strings.TrimPrefix(img.ID, "sha256:")[:12])
	}
	if inUse(img.ID) {
		return Removal{}, fmt.Errorf("%w: unable to remove image %s: image is being used by a container", ErrConflict, ref)
	}
	for _, r := range img.refs() {
		if !contains(untagged, r) {
			untagged = append(untagged, r)
		}
	}
	if err := os.RemoveAll(s.Root(img.ID)); err != nil {
		return Removal{}, err
	}
	delete(s.idx, img.ID)
	if err := s.saveLocked(); err != nil {
		return Removal{}, err
	}
	s.blobs.Lock()
	defer s.blobs.Unlock()
	return Removal{Image: *img, Untagged: untagged, Deleted: true}, s.collectLocked()
}

func hasTagIn(tags []string, repo string) bool {
	for _, t := range tags {
		if Repository(t) == repo {
			return true
		}
	}
	return false
}

func repos(refs []string) map[string]bool {
	out := map[string]bool{}
	for _, r := range refs {
		out[Repository(r)] = true
	}
	return out
}

func contains(list []string, v string) bool {
	for _, t := range list {
		if t == v {
			return true
		}
	}
	return false
}

// Progress receives a pull's progress as Docker reports it: id is a layer's
// short digest, status what happened to it.
type Progress func(id, status string)

// Pulled is the outcome of a pull.
type Pulled struct {
	Image Image
	// Ref is the normalized reference pulled.
	Ref string
	// Digest is what the reference resolved to in the registry: the image's
	// manifest, or the index the engine's platform was chosen from.
	Digest string
	// UpToDate reports that the reference already named this image.
	UpToDate bool
}

// Pull fetches the image for the engine's platform from its registry with
// the client's credentials, as dockerd pulls with those the CLI sends,
// downloading only the layers the store lacks, and records the reference
// and the repository@digest it resolved to.
func (s *Store) Pull(ctx context.Context, ref string, auth authn.Authenticator, progress Progress) (Pulled, error) {
	r, err := name.ParseReference(ref, name.WithDefaultTag("latest"))
	if err != nil {
		return Pulled{}, err
	}
	n, err := Normalize(r.String())
	if err != nil {
		return Pulled{}, err
	}
	desc, err := remote.Get(r, remote.WithContext(ctx), remote.WithPlatform(Platform), remote.WithAuth(auth))
	if err != nil {
		return Pulled{}, err
	}
	img, err := desc.Image()
	if err != nil {
		return Pulled{}, err
	}
	refs := []string{Familiar(r.Context()) + "@" + desc.Digest.String()}
	if !isDigestRef(n) {
		refs = append([]string{n}, refs...)
	}
	before, beforeErr := s.Get(n)

	s.blobs.RLock()
	defer s.blobs.RUnlock()
	md, err := img.Digest()
	if err != nil {
		return Pulled{}, err
	}
	var fetched []string
	if beforeErr != nil || before.Manifest.Digest != md {
		if fetched, err = s.fetchLayers(img, progress); err != nil {
			return Pulled{}, err
		}
	}
	added, err := s.add(img, refs, "")
	if err != nil {
		return Pulled{}, err
	}
	for _, id := range fetched {
		progress(id, "Pull complete")
	}
	return Pulled{Image: added, Ref: n, Digest: desc.Digest.String(), UpToDate: beforeErr == nil && before.ID == added.ID}, nil
}

// fetchLayers downloads the image's layers the store lacks into the layout,
// reporting each, and returns the short IDs of those it downloaded.
func (s *Store) fetchLayers(img v1.Image, progress Progress) ([]string, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	var missing []v1.Layer
	var ids []string
	for _, l := range layers {
		dg, err := l.Digest()
		if err != nil {
			return nil, err
		}
		id := dg.Hex[:12]
		if _, err := os.Stat(s.blobPath(dg)); err == nil {
			progress(id, "Already exists")
			continue
		}
		progress(id, "Pulling fs layer")
		missing = append(missing, l)
		ids = append(ids, id)
	}
	for i, l := range missing {
		progress(ids[i], "Downloading")
		if err := s.writeLayer(l); err != nil {
			return nil, err
		}
		progress(ids[i], "Download complete")
	}
	return ids, nil
}

// writeLayer writes a layer's compressed blob into the layout through a
// temporary file, checking its digest before it takes the blob's name.
func (s *Store) writeLayer(l v1.Layer) error {
	dg, err := l.Digest()
	if err != nil {
		return err
	}
	rc, err := l.Compressed()
	if err != nil {
		return err
	}
	defer rc.Close()
	dir := filepath.Dir(s.blobPath(dg))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".download-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), rc)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != dg.Hex {
		return fmt.Errorf("layer %s downloaded with digest sha256:%s", dg, got)
	}
	return os.Rename(tmp.Name(), s.blobPath(dg))
}

// Load imports every image in a `docker save` archive: an OCI image
// layout, which Docker writes alongside its manifest.json and which keeps
// manifests byte for byte, or a manifest.json archive alone.
func (s *Store) Load(r io.Reader) ([]Image, error) {
	tmp, err := os.CreateTemp(s.dir, "load-*.tar")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, r); err != nil {
		return nil, err
	}
	names, err := archiveNames(tmp.Name())
	if err != nil {
		return nil, err
	}
	if names["index.json"] && names["oci-layout"] {
		return s.loadLayout(tmp.Name())
	}
	if !names["manifest.json"] {
		return nil, errors.New("archive has neither an OCI index.json nor a manifest.json")
	}
	return s.loadManifest(tmp.Name())
}

func archiveNames(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]bool{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out[filepath.Clean(h.Name)] = true
	}
}

func (s *Store) loadLayout(archivePath string) ([]Image, error) {
	dir, err := os.MkdirTemp(s.dir, "load-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	err = Extract(dir, f, false)
	f.Close()
	if err != nil {
		return nil, err
	}
	idx, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		return nil, err
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	type entry struct {
		img  v1.Image
		tags []string
	}
	var order []v1.Hash
	found := map[v1.Hash]*entry{}
	for _, d := range im.Manifests {
		img, err := platformImage(idx, d)
		if err != nil {
			return nil, err
		}
		dg, err := img.Digest()
		if err != nil {
			return nil, err
		}
		e, ok := found[dg]
		if !ok {
			e = &entry{img: img}
			found[dg] = e
			order = append(order, dg)
		}
		if t := annotatedName(d.Annotations); t != "" {
			n, err := Normalize(t)
			if err != nil {
				return nil, err
			}
			e.tags = append(e.tags, n)
		}
	}
	var out []Image
	for _, dg := range order {
		added, err := s.Add(found[dg].img, found[dg].tags, "")
		if err != nil {
			return nil, err
		}
		out = append(out, added)
	}
	return out, nil
}

// annotatedName is the image name an index descriptor carries:
// containerd's full name, or a ref name that is a whole reference rather
// than a bare tag.
func annotatedName(a map[string]string) string {
	if n := a[AnnotationImageName]; n != "" {
		return n
	}
	if n := a[AnnotationRefName]; strings.ContainsAny(n, "/:") {
		return n
	}
	return ""
}

// platformImage resolves an index descriptor to the image for the engine's
// platform: the image itself, or its entry in a nested index.
func platformImage(idx v1.ImageIndex, d v1.Descriptor) (v1.Image, error) {
	switch {
	case d.MediaType.IsImage():
		return idx.Image(d.Digest)
	case d.MediaType.IsIndex():
		child, err := idx.ImageIndex(d.Digest)
		if err != nil {
			return nil, err
		}
		cm, err := child.IndexManifest()
		if err != nil {
			return nil, err
		}
		for _, c := range cm.Manifests {
			if c.MediaType.IsImage() && c.Platform != nil && c.Platform.OS == Platform.OS && c.Platform.Architecture == Platform.Architecture {
				return child.Image(c.Digest)
			}
		}
		return nil, fmt.Errorf("index %s has no image for %s/%s", d.Digest, Platform.OS, Platform.Architecture)
	}
	return nil, fmt.Errorf("index entry %s has media type %s, not an image", d.Digest, d.MediaType)
}

func (s *Store) loadManifest(path string) ([]Image, error) {
	opener := func() (io.ReadCloser, error) { return os.Open(path) }
	manifest, err := readManifest(path)
	if err != nil {
		return nil, err
	}
	var out []Image
	for _, m := range manifest {
		var tags []string
		var tagRef *name.Tag
		for _, t := range m.RepoTags {
			n, err := Normalize(t)
			if err != nil {
				return nil, err
			}
			tags = append(tags, n)
			if tagRef == nil {
				tr, err := name.NewTag(t)
				if err != nil {
					return nil, err
				}
				tagRef = &tr
			}
		}
		img, err := tarball.Image(opener, tagRef)
		if err != nil {
			return nil, err
		}
		added, err := s.Add(img, tags, "")
		if err != nil {
			return nil, err
		}
		out = append(out, added)
	}
	return out, nil
}

type manifestEntry struct {
	Config   string
	RepoTags []string
	Layers   []string
}

func readManifest(path string) ([]manifestEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("archive has no manifest.json")
		}
		if err != nil {
			return nil, err
		}
		if filepath.Clean(h.Name) == "manifest.json" {
			var m []manifestEntry
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return nil, fmt.Errorf("manifest.json: %w", err)
			}
			if len(m) == 0 {
				return nil, errors.New("manifest.json lists no images")
			}
			return m, nil
		}
	}
}

// Add stores img under refs, normalized tags or repository@digest
// references. Its blobs go into the layout as they are, so its manifest
// digest stays the same. root, when set, is the image already unpacked,
// which the store takes over; otherwise the store unpacks the layers.
func (s *Store) Add(img v1.Image, refs []string, root string) (Image, error) {
	s.blobs.RLock()
	defer s.blobs.RUnlock()
	return s.add(img, refs, root)
}

// add is Add with the blobs lock held shared.
func (s *Store) add(img v1.Image, refs []string, root string) (Image, error) {
	discard := func() {
		if root != "" {
			os.RemoveAll(root)
		}
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		discard()
		return Image{}, err
	}
	if cfg.OS != Platform.OS || cfg.Architecture != Platform.Architecture {
		discard()
		return Image{}, fmt.Errorf("image platform (%s/%s) does not match the engine platform (%s/%s)", cfg.OS, cfg.Architecture, Platform.OS, Platform.Architecture)
	}
	digest, err := img.ConfigName()
	if err != nil {
		discard()
		return Image{}, err
	}
	id := digest.String()
	if out, ok, err := s.retag(id, refs); ok {
		discard()
		return out, err
	}
	rec, err := s.unpack(img, root)
	if err != nil {
		return Image{}, err
	}
	rec.ID, rec.Created, rec.Config = id, cfg.Created.Time, *cfg
	if raw, err := img.RawConfigFile(); err == nil {
		rec.HealthStartInterval = healthStartInterval(raw)
	}

	if out, ok, err := s.retag(id, refs); ok {
		// Another add stored the same image meanwhile.
		os.RemoveAll(rec.stage)
		return out, err
	}
	usage := DiskUsage(rec.stage)
	rec.RootUsage = &usage
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(rec.stage, s.Root(id)); err != nil {
		os.RemoveAll(rec.stage)
		return Image{}, err
	}
	rec.Image.RepoTags, rec.Image.RepoDigests = []string{}, []string{}
	s.idx[id] = &rec.Image
	s.addRefsLocked(&rec.Image, refs)
	return rec.Image, s.saveLocked()
}

// retag adds refs to the image id when the store holds it already.
func (s *Store) retag(id string, refs []string) (Image, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.idx[id]
	if !ok {
		return Image{}, false, nil
	}
	s.addRefsLocked(existing, refs)
	return *existing, true, s.saveLocked()
}

type unpacked struct {
	Image
	stage string
}

// unpack writes img's blobs into the layout and measures its layers,
// applying them onto a fresh root unless root is the image already
// unpacked. It removes the root on failure.
func (s *Store) unpack(img v1.Image, root string) (_ unpacked, err error) {
	stage := root
	defer func() {
		if err != nil && stage != "" {
			os.RemoveAll(stage)
		}
	}()
	if err := s.layout().WriteImage(img); err != nil {
		return unpacked{}, err
	}
	dg, err := img.Digest()
	if err != nil {
		return unpacked{}, err
	}
	raw, err := img.RawManifest()
	if err != nil {
		return unpacked{}, err
	}
	mt, err := img.MediaType()
	if err != nil {
		return unpacked{}, err
	}
	desc := v1.Descriptor{MediaType: mt, Digest: dg, Size: int64(len(raw)), Platform: &v1.Platform{OS: Platform.OS, Architecture: Platform.Architecture}}
	local, err := s.stored(desc)
	if err != nil {
		return unpacked{}, err
	}
	if stage == "" {
		if stage, err = os.MkdirTemp(filepath.Join(s.dir, "roots"), ".stage-"); err != nil {
			return unpacked{}, err
		}
	}
	out := unpacked{Image: Image{Manifest: desc, LayerSizes: []int64{}}, stage: stage}
	layers, err := local.Layers()
	if err != nil {
		return unpacked{}, err
	}
	for _, l := range layers {
		n, err := measure(l, stage, root == "")
		if err != nil {
			return unpacked{}, err
		}
		out.LayerSizes = append(out.LayerSizes, n)
		out.Size += n
	}
	return out, nil
}

// measure reads a layer's uncompressed stream, applying it onto dir when
// apply is set, and returns its length.
func measure(l v1.Layer, dir string, apply bool) (int64, error) {
	rc, err := uncompressed(l)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n := &countingReader{r: rc}
	if apply {
		if err := ApplyLayer(dir, n); err != nil {
			return 0, err
		}
	}
	if _, err := io.Copy(io.Discard, n); err != nil {
		return 0, err
	}
	return n.n, nil
}

// uncompressed opens a layer's tar stream, decompressing a gzip layer
// with klauspost's decoder, which reads several times faster than the
// standard library's.
func uncompressed(l v1.Layer) (io.ReadCloser, error) {
	mt, err := l.MediaType()
	if err != nil {
		return nil, err
	}
	if mt != types.OCILayer && mt != types.DockerLayer {
		return l.Uncompressed()
	}
	rc, err := l.Compressed()
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(rc)
	if err != nil {
		rc.Close()
		return nil, err
	}
	return gzipReadCloser{gz, rc}, nil
}

type gzipReadCloser struct {
	*gzip.Reader
	compressed io.ReadCloser
}

func (g gzipReadCloser) Close() error {
	err := g.Reader.Close()
	if cerr := g.compressed.Close(); err == nil {
		err = cerr
	}
	return err
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// V1Image reads a stored image back as the OCI image it was stored as.
func (s *Store) V1Image(ref string) (v1.Image, Image, error) {
	rec, err := s.Get(ref)
	if err != nil {
		return nil, Image{}, err
	}
	img, err := s.stored(rec.Manifest)
	if err != nil {
		return nil, Image{}, err
	}
	return img, rec, nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.indexPath()); err != nil {
		return err
	}
	return s.writeIndexLocked()
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
