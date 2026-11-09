package image

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func flagged(t *testing.T, p string) bool {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return st.Flags&MetaFlag != 0
}

// Extraction keeps the owner's read and write on disk and records the
// archive's mode, which archives of the files carry again.
func TestExtractRecordsNarrowModes(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "ro/", Typeflag: tar.TypeDir, Mode: 0o555})
	tw.WriteHeader(&tar.Header{Name: "ro/file", Typeflag: tar.TypeReg, Mode: 0o444, Size: 2})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: "rw", Typeflag: tar.TypeReg, Mode: 0o640, Size: 0})
	tw.Close()
	dir := t.TempDir()
	if err := Extract(dir, &b, false); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][2]os.FileMode{"ro": {0o755, 0o555}, "ro/file": {0o644, 0o444}, "rw": {0o640, 0o640}} {
		p := filepath.Join(dir, name)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want[0] || RecordedMode(p, fi) != want[1] {
			t.Errorf("%s: on disk %o, recorded %o; want %o, %o", name, fi.Mode().Perm(), RecordedMode(p, fi), want[0], want[1])
		}
		// The library reads a recorded mode only from a flagged file.
		if got := flagged(t, p); got != (want[0] != want[1]) {
			t.Errorf("%s: flagged %v", name, got)
		}
	}
}

// MarkTree flags the files an earlier engine recorded an owner or a mode
// on, a link among them, and leaves the rest.
func TestMarkTreeFlagsRecordedFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"mode", "owner", "plain"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("plain", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	unix.Lsetxattr(filepath.Join(dir, "mode"), ModeXattr, []byte("444"), 0)
	unix.Lsetxattr(filepath.Join(dir, "owner"), ownerXattr, []byte("999:999"), 0)
	if err := unix.Lsetxattr(filepath.Join(dir, "link"), ownerXattr, []byte("999:999"), 0); err != nil {
		t.Fatal(err)
	}
	if err := MarkTree(dir); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]bool{"mode": true, "owner": true, "link": true, "plain": false} {
		if got := flagged(t, filepath.Join(dir, f)); got != want {
			t.Errorf("%s: flagged %v, want %v", f, got, want)
		}
	}
}
