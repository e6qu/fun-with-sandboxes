package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestList(t *testing.T) {
	binDir = t.TempDir()
	for _, n := range []string{"wget", "sh", "busybox", "awk"} {
		if err := os.WriteFile(filepath.Join(binDir, n), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := commands()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"awk", "sh", "wget"}; !reflect.DeepEqual(got, want) {
		t.Errorf("commands: %q, want %q", got, want)
	}
	if code := run([]string{"--list"}); code != 0 {
		t.Errorf("--list: exit %d", code)
	}
}

func TestUnknownApplets(t *testing.T) {
	binDir = t.TempDir()
	for _, args := range [][]string{{"missing"}, {"busybox"}, {"../sh"}, {"/bin/sh"}} {
		if code := run(args); code != 127 {
			t.Errorf("%q: exit %d, want 127", args, code)
		}
	}
	if code := run(nil); code != 1 {
		t.Errorf("no arguments: exit %d, want 1", code)
	}
}
