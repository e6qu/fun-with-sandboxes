package runtime

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
	"github.com/e6qu/fun-with-sandboxes/internal/pathmap"
	"github.com/e6qu/fun-with-sandboxes/internal/roles"
	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
)

// Compiling a container's Seatbelt profile takes libsandbox about 15 ms;
// applying the compiled bytecode takes the kernel about 35 µs. So a
// container's profile is compiled once, kept beside its text with the
// text's hash, and applied as bytecode by each start whose text is the
// same: a restart compiles nothing, and the engine compiles a first start's
// profile while the container's supervisor starts.

// profileText is the profile Start applies for s: s's own, which lets the
// container execute the engine's helper binary and its anchor launcher.
func profileText(s Spec) (seatbelt.Spec, string, error) {
	p := s.Profile
	self, err := roles.Binary()
	if err != nil {
		return p, "", err
	}
	// Seatbelt matches resolved paths.
	if p.Helper, err = filepath.EvalSymlinks(self); err != nil {
		return p, "", err
	}
	if s.Library != "" {
		if l, err := filepath.EvalSymlinks(pathmap.Launcher(s.Library)); err == nil {
			p.Launcher = l
		}
	}
	text, err := seatbelt.Render(p)
	return p, text, err
}

// compiledPath is where the compiled profile of the profile at ProfilePath
// is kept.
func compiledPath(s Spec) string {
	return strings.TrimSuffix(s.ProfilePath, ".sb") + ".bin"
}

// Precompile compiles the profile Start will apply for s, unless a compiled
// profile of the same text is kept already, and returns once it holds the
// compiled profile's lock: a Start that comes meanwhile waits for the
// compile, rather than compiling too.
func Precompile(s Spec) error {
	_, text, err := profileText(s)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(text))
	path := compiledPath(s)
	if kept(path, sum) {
		return nil
	}
	lock, err := lockCompiled(path)
	if err != nil {
		return err
	}
	go func() {
		defer lock.Close()
		compileLocked(path, sum, text)
	}()
	return nil
}

// compiled returns the path of the compiled profile of text, compiling it
// unless it is kept already or a Precompile is compiling it.
func compiled(s Spec, text string) (string, error) {
	sum := sha256.Sum256([]byte(text))
	path := compiledPath(s)
	if kept(path, sum) {
		return path, nil
	}
	lock, err := lockCompiled(path)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if kept(path, sum) {
		return path, nil
	}
	return path, compileLocked(path, sum, text)
}

// lockCompiled takes the compiled profile's lock, waiting for whoever holds
// it; closing the file releases it.
func lockCompiled(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func compileLocked(path string, sum [sha256.Size]byte, text string) error {
	bc, err := libsystem.SandboxCompile(text)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(sum[:], bc...), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// kept reports whether the compiled profile at path is of the text whose
// hash is sum.
func kept(path string, sum [sha256.Size]byte) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var got [sha256.Size]byte
	if _, err := f.ReadAt(got[:], 0); err != nil {
		return false
	}
	return bytes.Equal(got[:], sum[:])
}

// applyCompiled confines the helper to the compiled profile at path, if it
// is of text; it reports false, having applied nothing, if not.
func applyCompiled(path, text string) bool {
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
	sum := sha256.Sum256([]byte(text))
	if err != nil || len(b) <= sha256.Size || !bytes.Equal(b[:sha256.Size], sum[:]) {
		return false
	}
	return libsystem.SandboxApply(b[sha256.Size:]) == nil
}
