package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
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
