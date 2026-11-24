package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// inputsDigest is a digest of everything the archive is made from: this
// program, its flags, the content of the files they name outside the Nix
// store, and the store paths by name, which Nix derives from all that
// makes them. Build directories differ between runs; what they hold does
// not, so a file is hashed by its content, never by its path.
func inputsDigest(specPath, closure string, pkgs, files []string, arch, tag string, env, links, removes []string) (string, error) {
	h := sha256.New()
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if err := hashTree(h, self); err != nil {
		return "", err
	}
	field := func(kind, v string) { fmt.Fprintf(h, "%s\x00%s\x00", kind, v) }
	field("arch", arch)
	field("tag", tag)
	for _, group := range []struct {
		kind string
		vs   []string
	}{{"pkg", pkgs}, {"env", env}, {"link", links}, {"remove", removes}} {
		for _, v := range group.vs {
			field(group.kind, v)
		}
	}
	field("spec", "")
	if err := hashTree(h, specPath); err != nil {
		return "", err
	}
	b, err := os.ReadFile(specPath)
	if err != nil {
		return "", err
	}
	var spec Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		return "", fmt.Errorf("%s: %w", specPath, err)
	}
	specFiles := make([]string, 0, len(spec.Files))
	for dst, src := range spec.Files {
		specFiles = append(specFiles, dst+"="+src)
	}
	sort.Strings(specFiles)
	if closure != "" {
		field("closure", "")
		if err := hashTree(h, closure); err != nil {
			return "", err
		}
	}
	for _, kv := range append(specFiles, files...) {
		dst, src, ok := strings.Cut(kv, "=")
		if !ok {
			return "", fmt.Errorf("-file %q: want image-path=host-path", kv)
		}
		field("file", dst)
		if err := hashTree(h, src); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashTree adds a file or directory tree to h: a store path by its name,
// anything else by each entry's path within it, type, mode, and content
// or link target.
func hashTree(h hash.Hash, root string) error {
	if root == storeDir || strings.HasPrefix(root, storeDir+"/") {
		fmt.Fprintf(h, "store\x00%s\x00", root)
		return nil
	}
	// copyTree follows a root that is a link, so its digest does too.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if root == storeDir || strings.HasPrefix(root, storeDir+"/") {
		fmt.Fprintf(h, "store\x00%s\x00", root)
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "entry\x00%s\x00%v\x00", rel, info.Mode())
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00", target)
		case d.Type().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			n, err := io.Copy(h, f)
			f.Close()
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "\x00%d\x00", n)
		}
		return nil
	})
}

// inputsPath is where the digest of an archive's inputs is kept, beside it.
func inputsPath(out string) string { return out + ".inputs" }

// current reports whether the archive at out was made from inputs digest.
func current(out, digest string) bool {
	if _, err := os.Stat(out); err != nil {
		return false
	}
	b, err := os.ReadFile(inputsPath(out))
	return err == nil && strings.TrimSpace(string(b)) == digest
}
