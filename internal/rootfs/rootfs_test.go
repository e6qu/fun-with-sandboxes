package rootfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJoinStaysInTheRoot(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755)
	os.Symlink("usr/bin", filepath.Join(root, "bin"))
	os.Symlink("/usr", filepath.Join(root, "abs"))
	os.Symlink("../../../..", filepath.Join(root, "usr", "up"))
	os.Symlink(outside, filepath.Join(root, "host"))
	os.Symlink("loop", filepath.Join(root, "loop"))
	for in, want := range map[string]string{
		"/bin/tool":        "usr/bin/tool",
		"bin":              "usr/bin",
		"/abs/bin/x":       "usr/bin/x",
		"/usr/up/etc/f":    "etc/f",
		"/../../etc":       "etc",
		"/host/secret":     strings.TrimPrefix(outside, "/") + "/secret",
		"/new/dir/file":    "new/dir/file",
		"/bin/../lib/libc": "usr/lib/libc",
	} {
		got, err := Join(root, in)
		if err != nil {
			t.Errorf("Join(%q): %v", in, err)
			continue
		}
		if got != filepath.Join(root, want) {
			t.Errorf("Join(%q) = %q, want %q", in, got, filepath.Join(root, want))
		}
	}
	if _, err := Join(root, "/loop"); err == nil {
		t.Error("a symlink loop resolved")
	}
}
