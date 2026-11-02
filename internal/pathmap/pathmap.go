// Package pathmap builds fwsroot, the library the engine loads into every
// container process to give it the container's view of paths, identity,
// users and groups, and hostname (see src/fwsroot.c). The engine compiles it
// from the source it embeds, with the host's C compiler, once per version.
package pathmap

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

//go:embed src/fwsroot.c
var source []byte

// HostnameVar names the hostname the library presents; package identity
// names the user and group.
const HostnameVar = "FWS_HOSTNAME"

// Build compiles the library into dir unless a build of this source is
// there already, and returns its path.
func Build(dir string) (string, error) {
	sum := sha256.Sum256(source)
	out := filepath.Join(dir, "fwsroot-"+hex.EncodeToString(sum[:8])+".dylib")
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(dir, "build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "fwsroot.c")
	if err := os.WriteFile(src, source, 0o644); err != nil {
		return "", err
	}
	lib := filepath.Join(work, "fwsroot.dylib")
	cmd := exec.Command("/usr/bin/xcrun", "clang", "-dynamiclib", "-O2", "-Wall", "-Werror", "-o", lib, src)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build the container library with clang, which the Xcode Command Line Tools provide (xcode-select --install): %v\n%s", err, b)
	}
	// Another engine may have built it meanwhile; either build is the same.
	if err := os.Rename(lib, out); err != nil {
		return "", err
	}
	return out, nil
}
