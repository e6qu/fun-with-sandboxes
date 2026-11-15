package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The digest names an archive's inputs by content: the same files in
// another build directory give the same digest, and a changed file, flag
// or store path another.
func TestInputsDigest(t *testing.T) {
	inputs := func(content string) (spec, entry string) {
		dir := t.TempDir()
		spec = filepath.Join(dir, "image.json")
		if err := os.WriteFile(spec, []byte(`{"tag":"fws/x:1","config":{}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		entry = filepath.Join(dir, "entry")
		if err := os.WriteFile(entry, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		return spec, entry
	}
	digest := func(spec, entry string, pkgs ...string) string {
		d, err := inputsDigest(spec, "", pkgs, []string{"/entry=" + entry}, "arm64", "", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	a := digest(inputs("v1"))
	if b := digest(inputs("v1")); a != b {
		t.Errorf("the same inputs in another directory: %s, want %s", b, a)
	}
	if b := digest(inputs("v2")); a == b {
		t.Error("a changed file left the digest as it was")
	}
	spec, entry := inputs("v1")
	if digest(spec, entry, "/nix/store/aaa-x") == digest(spec, entry, "/nix/store/bbb-x") {
		t.Error("another store path left the digest as it was")
	}
}

// An archive is current where the digest recorded beside it matches.
func TestCurrent(t *testing.T) {
	out := filepath.Join(t.TempDir(), "x.tar")
	if current(out, "d") {
		t.Fatal("a missing archive is current")
	}
	os.WriteFile(out, []byte("archive"), 0o644)
	if current(out, "d") {
		t.Fatal("an archive without a recorded digest is current")
	}
	os.WriteFile(inputsPath(out), []byte("d\n"), 0o644)
	if !current(out, "d") || current(out, "e") {
		t.Fatal("the recorded digest is not compared")
	}
}
