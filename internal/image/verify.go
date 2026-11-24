package image

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// hasher returns a hash of the algorithm that names h.
func hasher(h v1.Hash) (hash.Hash, error) {
	switch h.Algorithm {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	}
	return nil, fmt.Errorf("unsupported digest algorithm %q", h.Algorithm)
}

// noLinks refuses an unpacked layout whose index.json, oci-layout or
// blobs are anything but files: the layout is read through those names,
// and a symbolic link among them would have the store read a file of the
// host's. Other entries, such as the directories of the format before
// Docker 25 with their links between layers, the layout never reads.
func noLinks(dir string) error {
	for _, name := range []string{"index.json", "oci-layout"} {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err == nil && !fi.Mode().IsRegular() {
			return fmt.Errorf("archive entry %s is not a file", name)
		}
	}
	err := filepath.WalkDir(filepath.Join(dir, "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			return fmt.Errorf("archive entry %s is not a file or directory", filepath.ToSlash(rel))
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// verifyBlobs checks that every blob of the OCI image layout at dir hashes
// to the digest it is stored under.
func verifyBlobs(dir string) error {
	root := filepath.Join(dir, "blobs")
	algs, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, alg := range algs {
		if !alg.IsDir() {
			return fmt.Errorf("archive blobs/%s: not a directory of blobs", alg.Name())
		}
		blobs, err := os.ReadDir(filepath.Join(root, alg.Name()))
		if err != nil {
			return err
		}
		for _, b := range blobs {
			want := v1.Hash{Algorithm: alg.Name(), Hex: b.Name()}
			if err := verifyBlob(filepath.Join(root, alg.Name(), b.Name()), want); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyBlob(path string, want v1.Hash) error {
	h, err := hasher(want)
	if err != nil {
		return fmt.Errorf("archive blob %s: %w", want, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("archive blob %s is not a file", want)
	}
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("archive blob %s: %w", want, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want.Hex {
		return fmt.Errorf("archive blob %s has digest %s:%s: the archive is corrupt", want, want.Algorithm, got)
	}
	return nil
}
