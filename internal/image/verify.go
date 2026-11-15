package image

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
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
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("archive blob %s: %w", want, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want.Hex {
		return fmt.Errorf("archive blob %s has digest %s:%s: the archive is corrupt", want, want.Algorithm, got)
	}
	return nil
}
