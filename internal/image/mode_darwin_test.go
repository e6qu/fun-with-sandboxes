package image

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

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
	}
}
