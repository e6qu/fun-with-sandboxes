package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// cpTree stands in for clonefile where the tests run without APFS.
func cpTree(src, dst string) error { return exec.Command("cp", "-a", src, dst).Run() }

func writeAt(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// listTree describes the tree at root in one line: directories, links and
// files with their content.
func listTree(t *testing.T, root string) string {
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
func TestMergeLayer(t *testing.T) {
	tmp := t.TempDir()
	l1, l2, l3, dst := filepath.Join(tmp, "l1"), filepath.Join(tmp, "l2"), filepath.Join(tmp, "l3"), filepath.Join(tmp, "dst")
	writeAt(t, l1, "etc/passwd", "root")
	writeAt(t, l1, "etc/gone", "x")
	writeAt(t, l1, "opt/old/a", "a")
	writeAt(t, l1, "opt/old/b", "b")
	writeAt(t, l1, "file-then-dir", "f")
	writeAt(t, l1, "dir-then-file/x", "x")
	os.Symlink("etc/passwd", filepath.Join(l1, "link"))

	// The second layer removes a file, empties a directory it then fills,
	// swaps a file and a directory, and adds a tree of its own.
	writeAt(t, l2, "etc/.wh.gone", "")
	writeAt(t, l2, "opt/old/.wh..wh..opq", "")
	writeAt(t, l2, "opt/old/c", "c")
	writeAt(t, l2, "file-then-dir/inside", "i")
	writeAt(t, l2, "dir-then-file", "now a file")
	writeAt(t, l2, "nix/store/abc/bin/tool", "tool")
	os.Symlink("tool", filepath.Join(l2, "nix/store/abc/bin/alias"))

	// The third replaces a file, and removes a tree whose mark lies in a
	// directory the tree so far lacks.
	writeAt(t, l3, "etc/passwd", "root2")
	writeAt(t, l3, "new/.wh.nothing", "")
	writeAt(t, l3, "new/f", "f")
	writeAt(t, l3, ".wh.link", "")

	os.Mkdir(dst, 0o755)
	for _, l := range []string{l1, l2, l3} {
		if err := MergeLayer(l, dst, cpTree); err != nil {
			t.Fatal(err)
		}
	}
	want := "dir-then-file=now a file etc/ etc/passwd=root2 file-then-dir/ file-then-dir/inside=i " +
		"new/ new/f=f nix/ nix/store/ nix/store/abc/ nix/store/abc/bin/ nix/store/abc/bin/alias->tool nix/store/abc/bin/tool=tool " +
		"opt/ opt/old/ opt/old/c=c"
	if got := listTree(t, dst); got != want {
		t.Errorf("merged tree:\n%s\nwant:\n%s", got, want)
	}
}

// A directory two layers hold takes the later layer's mode and time.
func TestMergeLayerDirMeta(t *testing.T) {
	tmp := t.TempDir()
	l1, l2, dst := filepath.Join(tmp, "l1"), filepath.Join(tmp, "l2"), filepath.Join(tmp, "dst")
	writeAt(t, l1, "d/a", "a")
	writeAt(t, l2, "d/b", "b")
	writeAt(t, l2, "d/.wh.a", "")
	os.Chmod(filepath.Join(l2, "d"), 0o700)
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(l2, "d"), when, when)
	os.Mkdir(dst, 0o755)
	for _, l := range []string{l1, l2} {
		if err := MergeLayer(l, dst, cpTree); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(filepath.Join(dst, "d"))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the shared directory's mode: %v %v", fi.Mode().Perm(), err)
	}
	if !fi.ModTime().Equal(when) {
		t.Errorf("the shared directory's time: %v, want %v", fi.ModTime(), when)
	}
	if got := listTree(t, dst); got != "d/ d/b=b" {
		t.Errorf("merged tree: %s", got)
	}
}

// A whiteout under a link in the tree removes nothing the link reaches.
func TestMergeLayerWhiteoutsStayInTheTree(t *testing.T) {
	tmp := t.TempDir()
	outside, l1, l2, dst := filepath.Join(tmp, "outside"), filepath.Join(tmp, "l1"), filepath.Join(tmp, "l2"), filepath.Join(tmp, "dst")
	writeAt(t, outside, "precious", "p")
	os.MkdirAll(l1, 0o755)
	os.Symlink(outside, filepath.Join(l1, "lib"))
	writeAt(t, l2, "lib/.wh.precious", "")
	writeAt(t, l2, "lib/.wh..wh..opq", "")
	os.Mkdir(dst, 0o755)
	for _, l := range []string{l1, l2} {
		if err := MergeLayer(l, dst, cpTree); err != nil {
			t.Fatal(err)
		}
	}
	if !exists(filepath.Join(outside, "precious")) {
		t.Fatal("a whiteout removed a file outside the tree")
	}
	if got := listTree(t, dst); got != "lib/" {
		t.Errorf("merged tree: %s", got)
	}
}
