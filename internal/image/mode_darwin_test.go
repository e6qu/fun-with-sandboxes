package image

import (
	"archive/tar"
	"bytes"
	"context"
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

// Applying a layer records its owners and its setuid, setgid and sticky
// bits for the container library, and a later layer naming a directory
// again replaces what an earlier one recorded on it.
func TestApplyLayerRecordsOwnersAndSpecialBits(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "home/", Typeflag: tar.TypeDir, Mode: 0o750, Uid: 1000, Gid: 1000})
	tw.WriteHeader(&tar.Header{Name: "tmp/", Typeflag: tar.TypeDir, Mode: 0o1777})
	tw.WriteHeader(&tar.Header{Name: "su", Typeflag: tar.TypeReg, Mode: 0o4755})
	tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "su", Uid: 5, Gid: 6})
	tw.Close()
	dir := t.TempDir()
	if err := ApplyLayer(dir, &b); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]struct {
		mode     os.FileMode
		uid, gid int
		owned    bool
	}{
		"home": {0o750, 1000, 1000, true},
		"tmp":  {0o777 | os.ModeSticky, 0, 0, false},
		"su":   {0o755 | os.ModeSetuid, 0, 0, false},
	} {
		p := filepath.Join(dir, name)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&(os.ModeSetuid|os.ModeSticky) != 0 {
			t.Errorf("%s: special bits on disk: %v", name, fi.Mode())
		}
		if got := RecordedMode(p, fi); got != want.mode {
			t.Errorf("%s: recorded mode %v, want %v", name, got, want.mode)
		}
		if u, g, ok := RecordedOwner(p); ok != want.owned || u != want.uid || g != want.gid {
			t.Errorf("%s: recorded owner %d:%d (%v), want %d:%d", name, u, g, ok, want.uid, want.gid)
		}
		if !flagged(t, p) {
			t.Errorf("%s: not flagged", name)
		}
	}
	if u, g, ok := RecordedOwner(filepath.Join(dir, "link")); !ok || u != 5 || g != 6 {
		t.Errorf("link: recorded owner %d:%d (%v)", u, g, ok)
	}
	// The value the container library parses.
	buf := make([]byte, 32)
	if n, err := unix.Lgetxattr(filepath.Join(dir, "su"), ModeXattr, buf); err != nil || string(buf[:n]) != "4755" {
		t.Errorf("su: %s = %q, %v", ModeXattr, buf[:n], err)
	}

	b.Reset()
	tw = tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "home/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.Close()
	if err := ApplyLayer(dir, &b); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "home")
	fi, _ := os.Lstat(p)
	if _, _, ok := RecordedOwner(p); ok || RecordedMode(p, fi) != 0o755 {
		t.Errorf("home kept the owner or mode of the layer below: %v", RecordedMode(p, fi))
	}
}

// A diff carries the modes and owners containers see, not the disk's, and
// a chown or chmod a container makes, which changes the record alone, is
// a change.
func TestDiffWritesRecordedModesAndOwners(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "ro/", Typeflag: tar.TypeDir, Mode: 0o555})
	tw.WriteHeader(&tar.Header{Name: "ro/chowned", Typeflag: tar.TypeReg, Mode: 0o644})
	tw.WriteHeader(&tar.Header{Name: "ro/chmodded", Typeflag: tar.TypeReg, Mode: 0o444})
	tw.WriteHeader(&tar.Header{Name: "ro/kept", Typeflag: tar.TypeReg, Mode: 0o444})
	tw.Close()
	tmp := t.TempDir()
	lower, upper := filepath.Join(tmp, "lower"), filepath.Join(tmp, "upper")
	if err := os.Mkdir(lower, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLayer(lower, &b); err != nil {
		t.Fatal(err)
	}
	if err := CloneDir(lower, upper); err != nil {
		t.Fatal(err)
	}
	// What the container library does for chown 1000:1000 and chmod 644.
	if err := unix.Lsetxattr(filepath.Join(upper, "ro/chowned"), ownerXattr, []byte("1000:1000"), 0); err != nil {
		t.Fatal(err)
	}
	if err := Mark(filepath.Join(upper, "ro/chowned")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lremovexattr(filepath.Join(upper, "ro/chmodded"), ModeXattr); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Diff(context.Background(), lower, upper, &out); err != nil {
		t.Fatal(err)
	}
	got := map[string][3]int64{}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		got[h.Name] = [3]int64{h.Mode & 0o7777, int64(h.Uid), int64(h.Gid)}
	}
	want := map[string][3]int64{
		"ro/":         {0o555, 0, 0},
		"ro/chowned":  {0o644, 1000, 1000},
		"ro/chmodded": {0o644, 0, 0},
	}
	if len(got) != len(want) {
		t.Errorf("diff holds %v, want %v", got, want)
	}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("%s: mode and owner %o, want %o", n, got[n], w)
		}
	}
}
