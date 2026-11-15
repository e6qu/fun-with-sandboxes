package buildkit

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// copyTree stands in for clonefile where the tests run without APFS.
func copyTree(src, dst string) error { return exec.Command("cp", "-a", src, dst).Run() }

func tree(t *testing.T, root string) string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case d.IsDir():
			out = append(out, rel+"/")
		case d.Type()&os.ModeSymlink != 0:
			to, _ := os.Readlink(p)
			out = append(out, rel+"->"+to)
		default:
			b, _ := os.ReadFile(p)
			out = append(out, rel+"="+string(b))
		}
		return nil
	})
	sort.Strings(out)
	return strings.Join(out, " ")
}

// Layers kept apart, each with its whiteouts as files, put together in
// order give the tree that applying each onto the last gives.
func TestMergeDelta(t *testing.T) {
	tmp := t.TempDir()
	l1, l2, l3, dst := filepath.Join(tmp, "l1"), filepath.Join(tmp, "l2"), filepath.Join(tmp, "l3"), filepath.Join(tmp, "dst")
	write(t, l1, "etc/passwd", "root")
	write(t, l1, "etc/gone", "x")
	write(t, l1, "opt/old/a", "a")
	write(t, l1, "opt/old/b", "b")
	write(t, l1, "file-then-dir", "f")
	write(t, l1, "dir-then-file/x", "x")
	os.Symlink("etc/passwd", filepath.Join(l1, "link"))

	// The second layer removes a file, empties a directory it then fills,
	// swaps a file and a directory, and adds a tree of its own.
	write(t, l2, "etc/.wh.gone", "")
	write(t, l2, "opt/old/.wh..wh..opq", "")
	write(t, l2, "opt/old/c", "c")
	write(t, l2, "file-then-dir/inside", "i")
	write(t, l2, "dir-then-file", "now a file")
	write(t, l2, "nix/store/abc/bin/tool", "tool")
	os.Symlink("tool", filepath.Join(l2, "nix/store/abc/bin/alias"))

	// The third replaces a file, and removes a tree whose mark lies in a
	// directory the tree so far lacks.
	write(t, l3, "etc/passwd", "root2")
	write(t, l3, "new/.wh.nothing", "")
	write(t, l3, "new/f", "f")
	write(t, l3, ".wh.link", "")

	os.Mkdir(dst, 0o755)
	for _, l := range []string{l1, l2, l3} {
		if err := mergeDelta(l, dst, copyTree); err != nil {
			t.Fatal(err)
		}
	}
	want := "dir-then-file=now a file etc/ etc/passwd=root2 file-then-dir/ file-then-dir/inside=i " +
		"new/ new/f=f nix/ nix/store/ nix/store/abc/ nix/store/abc/bin/ nix/store/abc/bin/alias->tool nix/store/abc/bin/tool=tool " +
		"opt/ opt/old/ opt/old/c=c"
	if got := tree(t, dst); got != want {
		t.Errorf("merged tree:\n%s\nwant:\n%s", got, want)
	}
}

// A directory two layers hold takes the later layer's mode.
func TestMergeDeltaDirMode(t *testing.T) {
	tmp := t.TempDir()
	l1, l2, dst := filepath.Join(tmp, "l1"), filepath.Join(tmp, "l2"), filepath.Join(tmp, "dst")
	write(t, l1, "d/a", "a")
	write(t, l2, "d/b", "b")
	os.Chmod(filepath.Join(l2, "d"), 0o700)
	os.Mkdir(dst, 0o755)
	for _, l := range []string{l1, l2} {
		if err := mergeDelta(l, dst, copyTree); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dst, "d")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the shared directory's mode: %v %v", fi.Mode().Perm(), err)
	}
}
