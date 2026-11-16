package image

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containerd/continuity/fs"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Applying a layer keeps the times it gives files, links and directories,
// a directory's even where the layer writes into it after naming it.
func TestApplyLayerKeepsTimes(t *testing.T) {
	when := func(s int64) time.Time { return time.Unix(1_600_000_000+s, 0) }
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: when(1)},
		{Name: "etc/conf", Typeflag: tar.TypeReg, Mode: 0o644, ModTime: when(2), Size: 1},
		{Name: "etc/link", Typeflag: tar.TypeSymlink, Linkname: "conf", ModTime: when(3)},
		{Name: "etc/sub/", Typeflag: tar.TypeDir, Mode: 0o555, ModTime: when(4)},
		{Name: "etc/sub/f", Typeflag: tar.TypeReg, Mode: 0o444, ModTime: when(5)},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size == 1 {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	dir := t.TempDir()
	if err := ApplyLayer(dir, &buf); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]time.Time{"etc": when(1), "etc/conf": when(2), "etc/link": when(3), "etc/sub": when(4), "etc/sub/f": when(5)} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(want) {
			t.Errorf("%s: mtime %v, want %v", name, fi.ModTime(), want)
		}
	}
	// The link's own time, not its target's.
	if fi, _ := os.Stat(filepath.Join(dir, "etc/link")); !fi.ModTime().Equal(when(2)) {
		t.Errorf("setting the link's time changed its target's to %v", fi.ModTime())
	}
}

// rewriteArchive copies a tar archive, passing each file's content through
// edit, which names the entry.
func rewriteArchive(t *testing.T, archive []byte, edit func(name string, body []byte) []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	tr := tar.NewReader(bytes.NewReader(archive))
	tw := tar.NewWriter(&out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		body = edit(h.Name, body)
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	return out.Bytes()
}

// A layout archive whose blob does not hash to its name fails to load and
// leaves no blob under that name.
func TestLoadRefusesCorruptBlobs(t *testing.T) {
	src := darwinImage(t, 2)
	var saved bytes.Buffer
	if err := WriteArchive(&saved, []ArchiveEntry{{Image: src, Tags: []string{"corrupt:1"}}}); err != nil {
		t.Fatal(err)
	}
	bad := digests(t, src)[1]
	corrupt := rewriteArchive(t, saved.Bytes(), func(n string, body []byte) []byte {
		if n == "blobs/sha256/"+bad.Hex {
			body = append([]byte(nil), body...)
			body[len(body)-1] ^= 0xff
		}
		return body
	})
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(bytes.NewReader(corrupt)); err == nil || !strings.Contains(err.Error(), bad.Hex) {
		t.Fatalf("loading a corrupt archive: %v", err)
	}
	if _, err := os.Stat(s.blobPath(bad)); !os.IsNotExist(err) {
		t.Errorf("the corrupt blob was stored: %v", err)
	}
	if n := len(s.List()); n != 0 {
		t.Errorf("the store holds %d images", n)
	}
	// The intact archive loads.
	if _, err := s.Load(bytes.NewReader(saved.Bytes())); err != nil {
		t.Fatal(err)
	}
}

// An image whose layers do not hash to its config's diff_ids, or that
// lists another number of them, fails to load.
func TestUnpackChecksDiffIDs(t *testing.T) {
	for kind, edit := range map[string]func(ids []v1.Hash) []v1.Hash{
		"wrong":   func(ids []v1.Hash) []v1.Hash { return []v1.Hash{ids[1], ids[0]} },
		"missing": func(ids []v1.Hash) []v1.Hash { return ids[:1] },
	} {
		t.Run(kind, func(t *testing.T) {
			src := darwinImage(t, 2)
			cfg, err := src.ConfigFile()
			if err != nil {
				t.Fatal(err)
			}
			cfg = cfg.DeepCopy()
			cfg.RootFS.DiffIDs = edit(cfg.RootFS.DiffIDs)
			img, err := mutate.ConfigFile(src, cfg)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			ref, _ := name.NewTag("diffids:1")
			if err := tarball.Write(ref, img, &buf); err != nil {
				t.Fatal(err)
			}
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(&buf); err == nil || !strings.Contains(err.Error(), "diff_id") {
				t.Fatalf("loading an image with %s diff_ids: %v", kind, err)
			}
			if n := len(s.List()); n != 0 {
				t.Errorf("the store holds %d images", n)
			}
		})
	}
}

func TestModeAndOwnerFormats(t *testing.T) {
	for _, m := range []os.FileMode{0o755, 0o755 | os.ModeSetuid, 0o555 | os.ModeSetgid, 0o777 | os.ModeSticky} {
		if got, ok := parseMode(formatMode(m)); !ok || got != m {
			t.Errorf("mode %v came back as %v, %v", m, got, ok)
		}
	}
	if TarMode(0o755|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o7755 || FileModeBits(0o104755) != 0o755|os.ModeSetuid {
		t.Error("tar mode bits")
	}
	// The container library reads the digits up to the first other byte.
	if m, ok := parseMode("2775\n"); !ok || m != 0o775|os.ModeSetgid {
		t.Errorf("parseMode = %v, %v", m, ok)
	}
	if u, g, ok := parseOwner(formatOwner(1000, 50)); !ok || u != 1000 || g != 50 {
		t.Errorf("owner came back as %d:%d, %v", u, g, ok)
	}
	if _, _, ok := parseOwner("nobody"); ok {
		t.Error("parsed an owner without a gid")
	}
}

// asRecorded writes the mode and owner the container sees over what the
// disk holds, and root where nothing is recorded.
func TestAsRecordedWritesRecordedMeta(t *testing.T) {
	var in bytes.Buffer
	tw := tar.NewWriter(&in)
	for _, h := range []*tar.Header{
		{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o40755, Uid: 501, Gid: 20},
		{Name: "dir/tool", Typeflag: tar.TypeReg, Mode: 0o100755, Uid: 501, Gid: 20, Size: 2},
		{Name: "dir/link", Typeflag: tar.TypeSymlink, Linkname: "tool", Mode: 0o120777, Uid: 501, Gid: 20},
		{Name: "plain", Typeflag: tar.TypeReg, Mode: 0o100644, Uid: 501, Gid: 20},
	} {
		tw.WriteHeader(h)
		if h.Size > 0 {
			tw.Write([]byte("#!"))
		}
	}
	tw.Close()
	meta := map[string]entryMeta{
		"dir/":     {mode: 0o555},
		"dir/tool": {mode: 0o755 | os.ModeSetuid, uid: 1000, gid: 1000},
		"dir/link": {mode: 0o777, uid: 7, gid: 8},
	}
	var out bytes.Buffer
	if err := asRecorded(&in, &out, func(n string) (entryMeta, bool) { m, ok := meta[n]; return m, ok }); err != nil {
		t.Fatal(err)
	}
	want := map[string][3]int64{
		"dir/":     {0o40555, 0, 0},
		"dir/tool": {0o104755, 1000, 1000},
		"dir/link": {0o120777, 7, 8},
		"plain":    {0o100644, 0, 0},
	}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := [3]int64{h.Mode, int64(h.Uid), int64(h.Gid)}; got != want[h.Name] {
			t.Errorf("%s: mode %o owner %d:%d, want %o %d:%d", h.Name, got[0], got[1], got[2], want[h.Name][0], want[h.Name][1], want[h.Name][2])
		}
	}
}

type change struct {
	kind fs.ChangeKind
	path string
}

func collect(t *testing.T, diff func(fs.ChangeFunc) error) []change {
	t.Helper()
	var out []change
	if err := diff(func(k fs.ChangeKind, p string, _ os.FileInfo, err error) error {
		out = append(out, change{k, p})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// Changes reports what continuity's fs.Changes reports, in its order, for
// trees whose times tell their files apart.
func TestChangesMatchContinuity(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	for _, d := range []string{lower, upper} {
		writeFile(t, d, "keep", "same")
		writeFile(t, d, "a/b/c", "c")
		writeFile(t, d, "a/b-c", "x")
		writeFile(t, d, "gone/deep/file", "g")
		writeFile(t, d, "dir-then-file/x", "x")
		writeFile(t, d, "file-then-dir", "f")
		writeFile(t, d, "hard", "h")
		os.Link(filepath.Join(d, "hard"), filepath.Join(d, "hard2"))
	}
	writeFile(t, upper, "a/b/c", "changed")
	writeFile(t, upper, "a/new", "n")
	os.RemoveAll(filepath.Join(upper, "gone"))
	os.RemoveAll(filepath.Join(upper, "dir-then-file"))
	writeFile(t, upper, "dir-then-file", "now a file")
	os.Remove(filepath.Join(upper, "file-then-dir"))
	writeFile(t, upper, "file-then-dir/inside", "i")
	os.Symlink("keep", filepath.Join(upper, "link"))
	// Same content, same times: unchanged.
	for _, d := range []string{lower, upper} {
		os.Chtimes(filepath.Join(d, "keep"), time.Unix(5, 7), time.Unix(5, 7))
	}

	ctx := context.Background()
	got := collect(t, func(fn fs.ChangeFunc) error { return Changes(ctx, lower, upper, nil, fn) })
	want := collect(t, func(fn fs.ChangeFunc) error { return fs.Changes(ctx, lower, upper, fn) })
	if len(got) < 8 || !reflect.DeepEqual(got, want) {
		t.Errorf("Changes reported\n%v\ncontinuity reports\n%v", got, want)
	}
}

// A file of the same size and whole-second time is compared by content,
// unless Settled vouches for it.
func TestWriteDiffAsksSettled(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	writeFile(t, lower, "f", "old")
	writeFile(t, upper, "f", "new")
	for _, d := range []string{lower, upper} {
		os.Chtimes(filepath.Join(d, "f"), time.Unix(1_600_000_000, 0), time.Unix(1_600_000_000, 0))
	}
	entries := func(settled Settled) []string {
		var buf bytes.Buffer
		if err := WriteDiff(context.Background(), &buf, lower, upper, DiffOptions{Settled: settled}); err != nil {
			t.Fatal(err)
		}
		var out []string
		tr := tar.NewReader(&buf)
		for {
			h, err := tr.Next()
			if err != nil {
				return out
			}
			out = append(out, h.Name)
		}
	}
	if got := entries(nil); len(got) != 1 || got[0] != "f" {
		t.Errorf("without Settled the diff holds %v", got)
	}
	asked := false
	if got := entries(func(rel string, l, u, ld, ud os.FileInfo) bool {
		asked = rel == "/f" && ld != nil && ud != nil
		return true
	}); len(got) != 0 || !asked {
		t.Errorf("with Settled the diff holds %v (asked with its path and directories: %v)", got, asked)
	}
}

func writeFile(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
