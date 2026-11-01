// Command fws-mkimage builds a darwin image archive that `docker load` accepts
// from a Nix closure, extra files, and an image config. The image is an OCI
// image with a layer per store path of the closure, so images that share
// store paths share those layers, and a last layer with everything else.
//
// Every absolute /nix/store reference a Mach-O file loads through, and every
// absolute symlink, is rewritten relative to the file itself, so the image is
// self-contained wherever its root lands on the host.
package main

import (
	"archive/tar"
	"debug/macho"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

type Spec struct {
	Tag string `json:"tag"`
	// Files maps a path in the image to a file on the build host.
	Files map[string]string `json:"files"`
	// Dirs are empty directories to create.
	Dirs   []string        `json:"dirs"`
	Config json.RawMessage `json:"config"`
}

const storeDir = "/nix/store"

func main() {
	specPath := flag.String("spec", "", "image spec JSON")
	closure := flag.String("closure", "", "file listing the Nix store paths of the closure, one per line")
	var pkgs multi
	flag.Var(&pkgs, "pkg", "store path whose bin/ entries are linked into /bin (repeatable)")
	var files multi
	flag.Var(&files, "file", "image-path=host-path to add (repeatable, overrides the spec)")
	out := flag.String("out", "", "output archive")
	arch := flag.String("arch", runtime.GOARCH, "image architecture")
	flag.Parse()
	if *specPath == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := build(*specPath, *closure, pkgs, files, *out, *arch); err != nil {
		log.Fatal(err)
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func build(specPath, closure string, pkgs, files []string, out, arch string) error {
	var spec Spec
	b, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return fmt.Errorf("%s: %w", specPath, err)
	}
	stage, err := os.MkdirTemp("", "fws-mkimage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	var paths []string
	if closure != "" {
		if paths, err = readLines(closure); err != nil {
			return err
		}
		for _, p := range paths {
			if err := copyStorePath(stage, p); err != nil {
				return fmt.Errorf("copy %s: %w", p, err)
			}
		}
		if err := relocate(stage); err != nil {
			return err
		}
		if err := verifyRelocated(stage); err != nil {
			return err
		}
	}
	for _, p := range pkgs {
		if err := linkBin(stage, p); err != nil {
			return err
		}
	}
	if spec.Files == nil {
		spec.Files = map[string]string{}
	}
	for _, f := range files {
		dst, src, ok := strings.Cut(f, "=")
		if !ok {
			return fmt.Errorf("-file %q: want image-path=host-path", f)
		}
		spec.Files[dst] = src
	}
	for dst, src := range spec.Files {
		if err := copyFile(src, filepath.Join(stage, dst)); err != nil {
			return err
		}
	}
	for _, d := range spec.Dirs {
		if err := os.MkdirAll(filepath.Join(stage, d), 0o755); err != nil {
			return err
		}
	}
	return writeArchive(stage, spec, paths, out, arch)
}

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// copyStorePath copies one store path into the stage, rewriting absolute
// symlinks into the store as relative ones. The store is read-only and
// deduplicated through hard links, so files are copied, not linked.
func copyStorePath(stage, storePath string) error {
	if !strings.HasPrefix(storePath, storeDir+"/") {
		return fmt.Errorf("%s is not a store path", storePath)
	}
	return filepath.WalkDir(storePath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(stage, p)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if strings.HasPrefix(target, "/") {
				if !strings.HasPrefix(target, storeDir+"/") {
					return fmt.Errorf("%s links outside the store to %s", p, target)
				}
				if target, err = filepath.Rel(filepath.Dir(p), target); err != nil {
					return err
				}
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			return os.Symlink(target, dst)
		case fi.IsDir():
			return os.MkdirAll(dst, 0o755)
		case fi.Mode().IsRegular():
			return copyFile(p, dst)
		}
		return nil
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm()|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// relocate rewrites every Mach-O file's absolute store references, then
// re-signs it ad hoc: arm64 macOS refuses to run code whose signature no
// longer matches.
func relocate(stage string) error {
	return filepath.WalkDir(filepath.Join(stage, storeDir), func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		refs, rpaths, ok, err := machoRefs(p)
		if err != nil || !ok {
			return err
		}
		var args []string
		for _, ref := range refs {
			if strings.HasPrefix(ref, storeDir+"/") {
				args = append(args, "-change", ref, loaderRel(stage, p, ref))
			}
		}
		for _, rp := range rpaths {
			if strings.HasPrefix(rp, storeDir+"/") {
				args = append(args, "-rpath", rp, loaderRel(stage, p, rp))
			}
		}
		if len(args) == 0 {
			return nil
		}
		if out, err := exec.Command("install_name_tool", append(args, p)...).CombinedOutput(); err != nil {
			return fmt.Errorf("install_name_tool %s: %v: %s", p, err, out)
		}
		if out, err := exec.Command("codesign", "--force", "--sign", "-", p).CombinedOutput(); err != nil {
			return fmt.Errorf("codesign %s: %v: %s", p, err, out)
		}
		return nil
	})
}

func loaderRel(stage, file, ref string) string {
	rel, err := filepath.Rel(filepath.Dir(file), filepath.Join(stage, ref))
	if err != nil {
		panic(err)
	}
	return "@loader_path/" + rel
}

// Load commands that name a dylib; debug/macho parses only LC_LOAD_DYLIB.
const (
	lcLoadDylib       = 0xc
	lcLoadWeakDylib   = 0x80000018
	lcReexportDylib   = 0x8000001f
	lcLazyLoadDylib   = 0x20
	lcLoadUpwardDylib = 0x80000023
	lcRpath           = 0x8000001c
)

// machoRefs lists the dylibs and rpaths a Mach-O file names; ok is false
// for anything that is not Mach-O.
func machoRefs(p string) (refs, rpaths []string, ok bool, err error) {
	files := []*macho.File{}
	if fat, ferr := macho.OpenFat(p); ferr == nil {
		defer fat.Close()
		for _, a := range fat.Arches {
			files = append(files, a.File)
		}
	} else if f, merr := macho.Open(p); merr == nil {
		defer f.Close()
		files = append(files, f)
	} else {
		return nil, nil, false, nil
	}
	seen := map[string]bool{}
	for _, f := range files {
		for _, l := range f.Loads {
			raw := l.Raw()
			if len(raw) < 12 {
				continue
			}
			cmd := f.ByteOrder.Uint32(raw[0:4])
			var dst *[]string
			switch cmd {
			case lcLoadDylib, lcLoadWeakDylib, lcReexportDylib, lcLazyLoadDylib, lcLoadUpwardDylib:
				dst = &refs
			case lcRpath:
				dst = &rpaths
			default:
				continue
			}
			// Both dylib_command and rpath_command hold the offset of their
			// name string right after cmd and cmdsize.
			off := f.ByteOrder.Uint32(raw[8:12])
			if int(off) >= len(raw) {
				return nil, nil, false, fmt.Errorf("%s: malformed load command 0x%x", p, cmd)
			}
			name := string(raw[off:])
			if i := strings.IndexByte(name, 0); i >= 0 {
				name = name[:i]
			}
			key := fmt.Sprintf("%t:%s", cmd == lcRpath, name)
			if !seen[key] {
				seen[key] = true
				*dst = append(*dst, name)
			}
		}
	}
	return refs, rpaths, true, nil
}

// verifyRelocated fails if any Mach-O file in the stage still loads through
// the host's store.
func verifyRelocated(stage string) error {
	return filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		refs, rpaths, ok, err := machoRefs(p)
		if err != nil || !ok {
			return err
		}
		for _, r := range append(refs, rpaths...) {
			if strings.HasPrefix(r, storeDir+"/") {
				return fmt.Errorf("%s still references %s after relocation", strings.TrimPrefix(p, stage), r)
			}
		}
		return nil
	})
}

// linkBin links each of a package's bin/ entries into /bin with a relative
// symlink.
func linkBin(stage, pkg string) error {
	src := filepath.Join(stage, pkg, "bin")
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("package %s: %w", pkg, err)
	}
	bin := filepath.Join(stage, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		rel, err := filepath.Rel(bin, filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		link := filepath.Join(bin, e.Name())
		os.Remove(link)
		if err := os.Symlink(rel, link); err != nil {
			return err
		}
	}
	return nil
}

// writeArchive writes the stage as an OCI image in the `docker save`
// layout: a layer per store path, in the closure's sorted order, then one
// with the rest of the stage.
func writeArchive(stage string, spec Spec, storePaths []string, out, arch string) error {
	dir, err := os.MkdirTemp("", "fws-mkimage-layers-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	epoch := v1.Time{Time: time.Unix(0, 0).UTC()}
	var adds []mutate.Addendum
	var history []v1.History
	var diffIDs []v1.Hash
	addLayer := func(createdBy string, include func(rel string) bool) error {
		p := filepath.Join(dir, fmt.Sprintf("%d.tar", len(adds)))
		if err := writeLayer(stage, p, include); err != nil {
			return err
		}
		l, err := tarball.LayerFromFile(p, tarball.WithMediaType(types.OCILayer))
		if err != nil {
			return err
		}
		d, err := l.DiffID()
		if err != nil {
			return err
		}
		adds = append(adds, mutate.Addendum{Layer: l, MediaType: types.OCILayer})
		diffIDs = append(diffIDs, d)
		history = append(history, v1.History{Created: epoch, CreatedBy: createdBy})
		return nil
	}
	sorted := append([]string(nil), storePaths...)
	sort.Strings(sorted)
	for _, sp := range sorted {
		own := strings.TrimPrefix(sp, "/")
		if err := addLayer("fws-mkimage: "+sp, func(rel string) bool {
			return rel == "nix" || rel == "nix/store" || rel == own || strings.HasPrefix(rel, own+"/")
		}); err != nil {
			return err
		}
	}
	if err := addLayer("fws-mkimage", func(rel string) bool {
		if len(sorted) > 0 && rel == "nix" {
			return false
		}
		return !strings.HasPrefix(rel+"/", "nix/store/")
	}); err != nil {
		return err
	}

	var cfg v1.Config
	if err := json.Unmarshal(spec.Config, &cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	img, err := mutate.Append(mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), types.OCIConfigJSON), adds...)
	if err != nil {
		return err
	}
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{
		Architecture: arch, OS: "darwin", Created: epoch, Config: cfg,
		RootFS: v1.RootFS{Type: "layers", DiffIDs: diffIDs}, History: history,
	})
	if err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := image.WriteArchive(f, []image.ArchiveEntry{{Image: img, Tags: []string{spec.Tag}}}); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeLayer writes the stage entries include accepts, by slash-separated
// path relative to the stage, as a deterministic layer tar.
func writeLayer(stage, path string, include func(rel string) bool) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	var paths []string
	err = filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(stage, p); p != stage && include(filepath.ToSlash(rel)) {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(stage, p)
		h.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		h.ModTime = time.Unix(0, 0)
		h.AccessTime, h.ChangeTime = time.Time{}, time.Time{}
		h.Format = tar.FormatPAX
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, in)
			in.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return f.Close()
}
