// Command fws-mkimage builds a darwin image archive that `docker load` accepts
// from a Nix closure, extra files, and an image config. The image is an OCI
// image with a layer per store path of the closure, so images that share
// store paths share those layers, and a last layer with everything else.
//
// Every absolute /nix/store reference a Mach-O file loads through, and every
// absolute symlink, is rewritten relative to the file itself, so the image is
// self-contained wherever its root lands on the host. A store path's layer
// depends on that store path alone, so it is the same in every image; what
// an image changes in a store path for its own /bin goes into its last
// layer.
package main

import (
	"archive/tar"
	"bytes"
	"debug/macho"
	"encoding/json"
	"errors"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

type Spec struct {
	Tag string `json:"tag"`
	// Files maps a path in the image to a file on the build host.
	Files map[string]string `json:"files"`
	// Dirs are empty directories to create.
	Dirs   []string        `json:"dirs"`
	Config json.RawMessage `json:"config"`
	// Env are variables set in the config, from -env, each replacing the
	// config's of its name.
	Env []string `json:"-"`
}

const storeDir = "/nix/store"

func main() {
	specPath := flag.String("spec", "", "image spec JSON")
	closure := flag.String("closure", "", "file listing the Nix store paths of the closure, one per line")
	var pkgs multi
	flag.Var(&pkgs, "pkg", "store path whose bin/ entries are linked into /bin (repeatable)")
	var files multi
	flag.Var(&files, "file", "image-path=host-path of a file or directory to add (repeatable, overrides the spec)")
	out := flag.String("out", "", "output archive")
	tag := flag.String("tag", "", "image tag, overriding the spec's")
	var env multi
	flag.Var(&env, "env", "KEY=value to add to the image's environment (repeatable)")
	var links multi
	flag.Var(&links, "link", "image-path=target of a symbolic link to add, such as /bin/sh=dash (repeatable)")
	var removes multi
	flag.Var(&removes, "remove", "image path to leave out of the image, such as a store path's file (repeatable)")
	var owns multi
	flag.Var(&owns, "own", "image-path=uid:gid of a directory the image holds, made if missing, that the user owns, such as an account's home (repeatable)")
	arch := flag.String("arch", runtime.GOARCH, "image architecture")
	reuse := flag.Bool("reuse", false, "keep the archive at -out where it was made from the same inputs, as <out>.inputs records them")
	flag.Parse()
	if *specPath == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	in := inputs{spec: *specPath, closure: *closure, pkgs: pkgs, files: files, out: *out, arch: *arch, tag: *tag, env: env, links: links, removes: removes, owns: owns}
	// The digest of the archive's inputs is recorded beside it once it is
	// made, and rewritten when it is kept, so a build can tell the
	// archives it made or kept from those an earlier catalog left.
	digest, err := inputsDigest(in)
	if err != nil {
		log.Fatal(err)
	}
	if *reuse && current(*out, digest) {
		log.Printf("%s: kept, made from the same inputs", *out)
	} else {
		os.Remove(inputsPath(*out))
		if err := build(in); err != nil {
			log.Fatal(err)
		}
	}
	if err := os.WriteFile(inputsPath(*out), []byte(digest+"\n"), 0o644); err != nil {
		log.Fatal(err)
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// inputs are what an image is made from: the flags, as main reads them.
type inputs struct {
	spec, closure             string
	pkgs, files               []string
	out, arch, tag            string
	env, links, removes, owns []string
}

// owner is a user and group that own a path of the image.
type owner struct{ uid, gid int }

// parseOwns reads -own's image-path=uid:gid entries, by the path as a
// layer names it, relative to the image's root.
func parseOwns(owns []string) (map[string]owner, error) {
	out := map[string]owner{}
	for _, o := range owns {
		at, ids, ok := strings.Cut(o, "=")
		u, g, ok2 := strings.Cut(ids, ":")
		uid, err1 := strconv.Atoi(u)
		gid, err2 := strconv.Atoi(g)
		if !ok || !ok2 || err1 != nil || err2 != nil || uid < 0 || gid < 0 || !strings.HasPrefix(at, "/") {
			return nil, fmt.Errorf("-own %q: want /image-path=uid:gid", o)
		}
		out[strings.TrimPrefix(filepath.Clean(at), "/")] = owner{uid, gid}
	}
	return out, nil
}

func build(in inputs) error {
	specPath, closure, pkgs, files, out, arch, tag, env, links, removes := in.spec, in.closure, in.pkgs, in.files, in.out, in.arch, in.tag, in.env, in.links, in.removes
	owners, err := parseOwns(in.owns)
	if err != nil {
		return err
	}
	var spec Spec
	b, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return fmt.Errorf("%s: %w", specPath, err)
	}
	if tag != "" {
		spec.Tag = tag
	}
	spec.Env = env
	stage, err := os.MkdirTemp("", "fws-mkimage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	// overlay holds the image's own versions of store path files, which
	// its last layer carries over the store paths' layers.
	overlay, err := os.MkdirTemp("", "fws-mkimage-overlay-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(overlay)

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
		if err := relocate(stage, filepath.Join(stage, storeDir)); err != nil {
			return err
		}
	}
	for _, p := range pkgs {
		if err := linkBin(stage, p); err != nil {
			return err
		}
	}
	if closure != "" {
		if err := rewriteShebangs(stage, overlay); err != nil {
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
	// Shorter paths first, so that a file added inside a directory that
	// is added too stands over that directory's, whatever the order given.
	dsts := make([]string, 0, len(spec.Files))
	for dst := range spec.Files {
		dsts = append(dsts, dst)
	}
	sort.Slice(dsts, func(i, j int) bool {
		a, b := filepath.Clean("/"+dsts[i]), filepath.Clean("/"+dsts[j])
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	})
	for _, dst := range dsts {
		src := spec.Files[dst]
		if err := outsideHostTrees("file", dst); err != nil {
			return err
		}
		if err := copyTree(src, filepath.Join(stage, dst)); err != nil {
			return err
		}
		// A file the image adds loads from the store as the closure's do:
		// dyld reads its absolute references on the host, before the
		// container library runs, so they are made relative too, to store
		// paths the image holds.
		if err := relocate(stage, filepath.Join(stage, dst)); err != nil {
			return err
		}
	}
	if err := verifyRelocated(stage); err != nil {
		return err
	}
	for _, d := range spec.Dirs {
		if err := outsideHostTrees("dir", d); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(stage, d), 0o755); err != nil {
			return err
		}
	}
	for rel := range owners {
		if err := outsideHostTrees("own", rel); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(stage, rel), 0o755); err != nil {
			return err
		}
	}
	for _, r := range removes {
		if err := os.Remove(filepath.Join(stage, r)); err != nil {
			return fmt.Errorf("-remove %s: %w", r, err)
		}
	}
	for _, l := range links {
		at, target, ok := strings.Cut(l, "=")
		if !ok || filepath.IsAbs(target) {
			return fmt.Errorf("-link %q: want image-path=relative-target", l)
		}
		if err := outsideHostTrees("link", at); err != nil {
			return err
		}
		p := filepath.Join(stage, at)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		os.Remove(p)
		if err := os.Symlink(target, p); err != nil {
			return err
		}
	}
	return writeArchive(stage, overlay, spec, paths, out, arch, owners)
}

// hostTrees are the trees the container library (fwsroot.c's host_trees)
// leaves to the host in every container: the dynamic loader and libSystem
// read them there. A file an image puts in one is in no container's view.
var hostTrees = []string{"/System", "/usr/lib", "/dev", "/private/var/db", "/var/db", "/Library/Apple"}

// outsideHostTrees refuses an image path in a host tree.
func outsideHostTrees(kind, p string) error {
	clean := filepath.Clean("/" + p)
	for _, t := range hostTrees {
		if clean == t || strings.HasPrefix(clean, t+"/") {
			return fmt.Errorf("%s %s: %s is the host's in every container, so the image's would never be seen", kind, p, t)
		}
	}
	return nil
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

// copyTree copies a file, or a directory and everything in it, following
// src where it is a link.
func copyTree(src, dst string) error {
	src, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return copyFile(src, dst)
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) {
				return fmt.Errorf("%s links to the absolute path %s, which no image holds", p, link)
			}
			return os.Symlink(link, target)
		default:
			return copyFile(p, target)
		}
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
	// A file the stage holds already, such as a store path's link that a
	// spec's file replaces, goes first rather than be written through.
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm()|0o200)
	if err != nil {
		return err
	}
	if n, err := io.Copy(out, in); err != nil {
		out.Close()
		// What the file system says on a failed write is often terse
		// (APFS has answered EILSEQ): say what was copied and how much
		// room the stage had left.
		return fmt.Errorf("copy %s (%d of %d bytes): %w; %s", src, n, fi.Size(), err, freeSpace(filepath.Dir(dst)))
	}
	return out.Close()
}

// freeSpace describes the room left on dir's file system.
func freeSpace(dir string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Sprintf("free space unknown: %v", err)
	}
	return fmt.Sprintf("%d MiB and %d inodes free there", st.Bavail*uint64(st.Bsize)>>20, st.Ffree)
}

// relocate rewrites every Mach-O file's absolute store references, then
// re-signs it ad hoc: arm64 macOS refuses to run code whose signature no
// longer matches.
func relocate(stage, dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
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
				if err := held(stage, p, ref); err != nil {
					return err
				}
				args = append(args, "-change", ref, loaderRel(stage, p, ref))
			}
		}
		for _, rp := range rpaths {
			if strings.HasPrefix(rp, storeDir+"/") {
				if err := held(stage, p, rp); err != nil {
					return err
				}
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
			return fmt.Errorf("codesign %s: %v: %s; %s", p, err, out, describeMachO(p))
		}
		return nil
	})
}

// describeMachO says what a file codesign refused looks like, and whether
// codesign takes it at a second try, so that a failure on a CI runner
// shows whether the file or the signing went wrong.
func describeMachO(p string) string {
	var b strings.Builder
	if fi, err := os.Stat(p); err == nil {
		fmt.Fprintf(&b, "%d bytes", fi.Size())
	}
	if f, err := os.Open(p); err == nil {
		head := make([]byte, 16)
		n, _ := io.ReadFull(f, head)
		f.Close()
		fmt.Fprintf(&b, ", starting %x", head[:n])
	}
	if out, err := exec.Command("lipo", "-info", p).CombinedOutput(); err == nil {
		fmt.Fprintf(&b, "; lipo: %s", strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("codesign", "--force", "--sign", "-", p).CombinedOutput(); err != nil {
		fmt.Fprintf(&b, "; a second codesign fails too: %s", strings.TrimSpace(string(out)))
	} else {
		b.WriteString("; a second codesign succeeds")
	}
	return b.String()
}

// held fails where the Mach-O file p loads ref, a store path's file or an
// rpath's directory, that the image does not hold: relative or not, the
// reference would find nothing in a container.
func held(stage, p, ref string) error {
	if _, err := os.Stat(filepath.Join(stage, ref)); err != nil {
		return fmt.Errorf("%s loads %s, which the image's closure does not hold", strings.TrimPrefix(p, stage), ref)
	}
	return nil
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

// systemShells are the host shells a container can execute; a script whose
// interpreter is one of them runs on the host's.
var systemShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true}

// rewriteShebangs makes the closure's scripts runnable in a container. The
// kernel resolves a script's interpreter on the host, where the image's
// store paths do not exist, so an interpreter the image puts on /bin is
// named through /usr/bin/env, which finds it on the container's PATH, and a
// shell becomes the host's, with the store paths in its script prefixed by
// the container's root, which every container process finds in
// SANDBOX_ROOT. A shell script's rewrite depends on the script alone and
// happens in place; one through /bin depends on what the image puts there,
// so it goes to overlay, at the script's path, for the image's last layer.
func rewriteShebangs(stage, overlay string) error {
	bins := map[string]bool{}
	if entries, err := os.ReadDir(filepath.Join(stage, "bin")); err == nil {
		for _, e := range entries {
			bins[e.Name()] = true
		}
	}
	return filepath.WalkDir(filepath.Join(stage, storeDir), func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil || !bytes.HasPrefix(b, []byte("#!"+storeDir+"/")) {
			return err
		}
		line, rest, _ := bytes.Cut(b, []byte("\n"))
		fields := strings.Fields(string(line[2:]))
		name := filepath.Base(fields[0])
		var head string
		dst := p
		switch {
		case systemShells[name]:
			head = "#!/bin/" + name
			// A shell script names programs by store path, which the
			// container's root holds under SANDBOX_ROOT.
			rest = bytes.ReplaceAll(rest, []byte(storeDir+"/"), []byte("${SANDBOX_ROOT}"+storeDir+"/"))
		case bins[name]:
			head = "#!/usr/bin/env -S " + name
			rel, err := filepath.Rel(stage, p)
			if err != nil {
				return err
			}
			dst = filepath.Join(overlay, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
		default:
			// An interpreter the image does not carry on /bin cannot run
			// the script in any container.
			return nil
		}
		if len(fields) > 1 {
			head += " " + strings.Join(fields[1:], " ")
		}
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, append([]byte(head+"\n"), rest...), fi.Mode().Perm())
	})
}

// sourceURL is where the images' sources live, for the manifest's
// org.opencontainers.image.source annotation.
const sourceURL = "https://github.com/e6qu/fun-with-sandboxes"

// writeArchive writes the stage as an OCI image in the `docker save`
// layout: a layer per store path, in the closure's sorted order, then one
// with the rest of the stage and what overlay holds over the store paths.
// It writes the epoch as every time, the image's creation included, so
// the same inputs give the same image.
func writeArchive(stage, overlay string, spec Spec, storePaths []string, out, arch string, owners map[string]owner) error {
	dir, err := os.MkdirTemp("", "fws-mkimage-layers-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	epoch := v1.Time{Time: time.Unix(0, 0).UTC()}
	var adds []mutate.Addendum
	var history []v1.History
	var diffIDs []v1.Hash
	addLayer := func(createdBy string, sources ...layerSource) error {
		p := filepath.Join(dir, fmt.Sprintf("%d.tar", len(adds)))
		if err := writeLayer(p, owners, sources...); err != nil {
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
		if err := addLayer("fws-mkimage: "+sp, layerSource{stage, func(rel string) bool {
			return rel == "nix" || rel == "nix/store" || rel == own || strings.HasPrefix(rel, own+"/")
		}}); err != nil {
			return err
		}
	}
	rest := layerSource{stage, func(rel string) bool {
		if len(sorted) > 0 && rel == "nix" {
			return false
		}
		return !strings.HasPrefix(rel+"/", "nix/store/")
	}}
	all := func(string) bool { return true }
	if err := addLayer("fws-mkimage", rest, layerSource{overlay, all}); err != nil {
		return err
	}

	var cfg v1.Config
	if err := json.Unmarshal(spec.Config, &cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// A variable -env names replaces the spec's of that name.
	for _, kv := range spec.Env {
		name, _, _ := strings.Cut(kv, "=")
		replaced := false
		for i, have := range cfg.Env {
			if n, _, _ := strings.Cut(have, "="); n == name {
				cfg.Env[i], replaced = kv, true
				break
			}
		}
		if !replaced {
			cfg.Env = append(cfg.Env, kv)
		}
	}
	img, err := mutate.Append(mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), types.OCIConfigJSON), adds...)
	if err != nil {
		return err
	}
	variant := ""
	if arch == "arm64" {
		variant = "v8"
	}
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{
		Architecture: arch, OS: "darwin", Variant: variant, Created: epoch, Config: cfg,
		RootFS: v1.RootFS{Type: "layers", DiffIDs: diffIDs}, History: history,
	})
	if err != nil {
		return err
	}
	img = mutate.Annotations(img, map[string]string{
		ocispec.AnnotationCreated: epoch.Format(time.RFC3339),
		ocispec.AnnotationSource:  sourceURL,
	}).(v1.Image)
	// go-containerregistry's config has no field for the health check's
	// start interval.
	var health struct {
		Healthcheck *struct{ StartInterval time.Duration }
	}
	if err := json.Unmarshal(spec.Config, &health); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if health.Healthcheck != nil && health.Healthcheck.StartInterval > 0 {
		if img, err = image.WithStartInterval(img, health.Healthcheck.StartInterval); err != nil {
			return err
		}
	}
	// The archive takes its name once whole: a catalog's directory loads
	// every *.tar in it, a half-written one too.
	f, err := os.CreateTemp(filepath.Dir(out), "."+filepath.Base(out)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = image.WriteArchive(f, []image.ArchiveEntry{{Image: img, Tags: []string{spec.Tag}}})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), out)
}

// layerSource is a tree a layer takes the entries include accepts from, by
// slash-separated path relative to dir.
type layerSource struct {
	dir     string
	include func(rel string) bool
}

// writeLayer writes the entries of sources as a deterministic layer tar,
// in lexical order of their paths; of entries at the same path, the
// later source's.
func writeLayer(path string, owners map[string]owner, sources ...layerSource) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	tw := tar.NewWriter(f)
	from := map[string]string{}
	for _, src := range sources {
		err = filepath.WalkDir(src.dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if rel, _ := filepath.Rel(src.dir, p); p != src.dir && src.include(filepath.ToSlash(rel)) {
				from[filepath.ToSlash(rel)] = p
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	rels := make([]string, 0, len(from))
	for rel := range from {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		p := from[rel]
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
		h.Name = rel
		if fi.IsDir() {
			h.Name += "/"
		}
		// Root owns what the image holds, but what -own gives a user.
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if o, ok := owners[rel]; ok {
			h.Uid, h.Gid = o.uid, o.gid
		}
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
	return tw.Close()
}
