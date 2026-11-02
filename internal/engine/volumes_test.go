package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckVolumeOptions(t *testing.T) {
	for _, c := range []struct {
		opts        map[string]string
		ok          bool
		unsupported bool
	}{
		{nil, true, false},
		{map[string]string{"type": "none", "o": "bind", "device": "/srv/data"}, true, false},
		{map[string]string{"o": "rbind", "device": "/srv/data"}, true, false},
		{map[string]string{"type": "tmpfs", "device": "tmpfs"}, true, false},
		{map[string]string{"type": "tmpfs", "device": "tmpfs", "o": "size=64m"}, false, true},
		{map[string]string{"type": "nfs", "o": "addr=10.0.0.1", "device": ":/export"}, false, true},
		{map[string]string{"type": "none", "o": "bind", "device": "relative"}, false, false},
		{map[string]string{"type": "none", "o": "bind,uid=1", "device": "/srv"}, false, true},
		{map[string]string{"type": "none", "device": "/srv"}, false, true},
		{map[string]string{"size": "1"}, false, false},
	} {
		err := checkVolumeOptions(c.opts)
		if (err == nil) != c.ok || (err != nil && (KindOf(err) == KindUnsupported) != c.unsupported) {
			t.Errorf("checkVolumeOptions(%v) = %v", c.opts, err)
		}
	}
}

// A subpath resolves inside its volume: a symlink in the volume cannot lead
// out of it.
func TestSubpathStaysInside(t *testing.T) {
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "inner"), 0o755)
	os.Symlink(outside, filepath.Join(dir, "escape"))
	os.Symlink("/inner", filepath.Join(dir, "absolute"))
	if got, err := subpath(dir, "inner"); err != nil || got != filepath.Join(dir, "inner") {
		t.Fatalf("subpath inner = %q, %v", got, err)
	}
	if got, err := subpath(dir, "absolute"); err != nil || got != filepath.Join(dir, "inner") {
		t.Fatalf("an absolute link resolves from the volume: %q, %v", got, err)
	}
	if _, err := subpath(dir, "escape"); err == nil {
		t.Fatal("a link out of the volume resolved")
	}
	if _, err := subpath(dir, "missing"); err == nil {
		t.Fatal("a missing subpath was accepted")
	}
}
