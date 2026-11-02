package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScriptCommand(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	os.WriteFile(filepath.Join(root, "bin", "sh"), []byte("\xcf\xfa\xed\xfe"), 0o755)
	script := filepath.Join(root, "run.sh")
	os.WriteFile(script, []byte("#!/bin/sh -e\necho hi\n"), 0o755)
	host := filepath.Join(root, "host.sh")
	os.WriteFile(host, []byte("#!/usr/bin/env node\n"), 0o755)
	binary := filepath.Join(root, "bin", "sh")

	path, argv := scriptCommand(root, script, []string{"/run.sh", "a", "b"})
	if path != filepath.Join(root, "bin", "sh") || !reflect.DeepEqual(argv, []string{"/bin/sh", "-e", script, "a", "b"}) {
		t.Errorf("script with the root's interpreter: %s %q", path, argv)
	}
	if path, argv := scriptCommand(root, host, []string{"/host.sh"}); path != host || len(argv) != 1 {
		t.Errorf("script with the host's interpreter: %s %q", path, argv)
	}
	if path, _ := scriptCommand(root, binary, []string{"sh"}); path != binary {
		t.Errorf("a binary ran as %s", path)
	}
}
