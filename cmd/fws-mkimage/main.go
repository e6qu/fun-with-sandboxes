// Command fws-mkimage builds a darwin image archive that `docker load` accepts
// from a Nix closure, extra files, and an image config.
//
// Every absolute /nix/store reference a Mach-O file loads through, and every
// absolute symlink, is rewritten relative to the file itself, so the image is
// self-contained wherever its root lands on the host.
package main

import (
	"archive/tar"
	"crypto/sha256"
	"debug/macho"
	"encoding/hex"
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

	if closure != "" {
		paths, err := readLines(closure)
		if err != nil {
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
	return writeArchive(stage, spec, out, arch)
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
		libs, err := f.ImportedLibraries()
		if err != nil {
			return nil, nil, false, err
		}
		for _, l := range libs {
			if !seen[l] {
				seen[l] = true
				refs = append(refs, l)
			}
		}
		for _, l := range f.Loads {
			if rp, isRpath := l.(*macho.Rpath); isRpath && !seen["rpath:"+rp.Path] {
				seen["rpath:"+rp.Path] = true
				rpaths = append(rpaths, rp.Path)
			}
		}
	}
	return refs, rpaths, true, nil
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

// writeArchive writes the stage as a single-layer image in the `docker save`
// layout: manifest.json, the config, and the layer.
func writeArchive(stage string, spec Spec, out, arch string) error {
	layerPath := out + ".layer"
	defer os.Remove(layerPath)
	if err := writeLayer(stage, layerPath); err != nil {
		return err
	}
	diffID, err := fileDigest(layerPath)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(spec.Config, &cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	image := map[string]any{
		"architecture": arch,
		"os":           "darwin",
		"created":      time.Unix(0, 0).UTC().Format(time.RFC3339),
		"config":       cfg,
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + diffID}},
		"history":      []map[string]any{{"created_by": "fws-mkimage"}},
	}
	cb, err := json.Marshal(image)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(cb)
	cfgName := hex.EncodeToString(sum[:]) + ".json"
	manifest, err := json.Marshal([]map[string]any{{"Config": cfgName, "RepoTags": []string{spec.Tag}, "Layers": []string{diffID + "/layer.tar"}}})
	if err != nil {
		return err
	}

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add("manifest.json", manifest); err != nil {
		return err
	}
	if err := add(cfgName, cb); err != nil {
		return err
	}
	lf, err := os.Open(layerPath)
	if err != nil {
		return err
	}
	defer lf.Close()
	li, _ := lf.Stat()
	if err := tw.WriteHeader(&tar.Header{Name: diffID + "/layer.tar", Mode: 0o644, Size: li.Size(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, lf); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return f.Close()
}

func writeLayer(stage, path string) error {
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
		if p != stage {
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

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
