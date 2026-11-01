package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// command encodes a dylib_command or rpath_command naming s, padded to 8 bytes.
func command(cmd uint32, s string) []byte {
	hdr := 24 // dylib_command: cmd, cmdsize, name offset, timestamp, versions
	if cmd == lcRpath {
		hdr = 12
	}
	size := (hdr + len(s) + 1 + 7) &^ 7
	b := make([]byte, size)
	binary.LittleEndian.PutUint32(b[0:], cmd)
	binary.LittleEndian.PutUint32(b[4:], uint32(size))
	binary.LittleEndian.PutUint32(b[8:], uint32(hdr))
	copy(b[hdr:], s)
	return b
}

func TestMachoRefsSeesEveryDylibCommand(t *testing.T) {
	cmds := [][]byte{
		command(lcLoadDylib, "/usr/lib/libSystem.B.dylib"),
		command(lcLoadWeakDylib, "/nix/store/aaa-weak/lib/libweak.dylib"),
		command(lcReexportDylib, "/nix/store/bbb-re/lib/libre.dylib"),
		command(lcRpath, "/nix/store/ccc-rp/lib"),
	}
	var body bytes.Buffer
	for _, c := range cmds {
		body.Write(c)
	}
	var f bytes.Buffer
	for _, v := range []uint32{0xfeedfacf, 0x0100000c, 0, 2, uint32(len(cmds)), uint32(body.Len()), 0, 0} {
		binary.Write(&f, binary.LittleEndian, v)
	}
	f.Write(body.Bytes())
	p := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(p, f.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	refs, rpaths, ok, err := machoRefs(p)
	if err != nil || !ok {
		t.Fatalf("machoRefs: ok=%v err=%v", ok, err)
	}
	for _, want := range []string{"/usr/lib/libSystem.B.dylib", "/nix/store/aaa-weak/lib/libweak.dylib", "/nix/store/bbb-re/lib/libre.dylib"} {
		if !slices.Contains(refs, want) {
			t.Errorf("refs %v miss %s", refs, want)
		}
	}
	if !slices.Equal(rpaths, []string{"/nix/store/ccc-rp/lib"}) {
		t.Errorf("rpaths %v", rpaths)
	}
	if err := verifyRelocated(filepath.Dir(p)); err == nil {
		t.Error("verifyRelocated accepted a file that still loads from /nix/store")
	}
}

func TestMachoRefsIgnoresOtherFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "script")
	os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0o755)
	if _, _, ok, err := machoRefs(p); ok || err != nil {
		t.Fatalf("script read as Mach-O: ok=%v err=%v", ok, err)
	}
}

// The archive has a layer per store path and one for the rest, and loads
// into the engine's store as the image the stage holds.
func TestWriteArchiveLayersPerStorePath(t *testing.T) {
	stage := t.TempDir()
	for p, body := range map[string]string{
		"nix/store/aaa-tool/bin/tool": "tool",
		"nix/store/bbb-lib/lib/l":     "lib",
		"entry":                       "entry",
	} {
		os.MkdirAll(filepath.Join(stage, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(stage, p), []byte(body), 0o755)
	}
	os.MkdirAll(filepath.Join(stage, "bin"), 0o755)
	os.Symlink("../nix/store/aaa-tool/bin/tool", filepath.Join(stage, "bin", "tool"))
	spec := Spec{Tag: "fws/test:1", Config: []byte(`{"Entrypoint":["/entry"]}`)}
	out := filepath.Join(t.TempDir(), "img.tar")
	if err := writeArchive(stage, spec, []string{"/nix/store/bbb-lib", "/nix/store/aaa-tool"}, out, runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
	s, err := image.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	loaded, err := s.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	img := loaded[0]
	if img.RepoTags[0] != "fws/test:1" || len(img.LayerSizes) != 3 || len(img.Config.History) != 3 {
		t.Fatalf("loaded %+v", img)
	}
	if img.Config.History[0].CreatedBy != "fws-mkimage: /nix/store/aaa-tool" || img.Config.Config.Entrypoint[0] != "/entry" {
		t.Fatalf("history %+v, config %+v", img.Config.History, img.Config.Config)
	}
	root := s.Root(img.ID)
	for _, p := range []string{"nix/store/aaa-tool/bin/tool", "nix/store/bbb-lib/lib/l", "entry", "bin/tool"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Fatalf("the loaded root lacks %s: %v", p, err)
		}
	}
	v, _, err := s.V1Image("fws/test:1")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := v.Manifest()
	if m.MediaType != "application/vnd.oci.image.manifest.v1+json" || m.Layers[0].MediaType != "application/vnd.oci.image.layer.v1.tar+gzip" {
		t.Fatalf("manifest %+v", m)
	}
}
