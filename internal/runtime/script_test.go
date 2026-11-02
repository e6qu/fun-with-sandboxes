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
	os.WriteFile(host, []byte("#!/usr/bin/perl\n"), 0o755)
	env := filepath.Join(root, "env.js")
	os.WriteFile(env, []byte("#!/usr/bin/env -S node --stack-size=100\n"), 0o755)
	os.WriteFile(filepath.Join(root, "bin", "node"), []byte("\xcf\xfa\xed\xfe"), 0o755)
	vars := []string{"PATH=/usr/bin:/bin"}
	binary := filepath.Join(root, "bin", "sh")

	path, argv := scriptCommand(root, script, []string{"/run.sh", "a", "b"}, vars)
	if path != filepath.Join(root, "bin", "sh") || !reflect.DeepEqual(argv, []string{"/bin/sh", "-e", script, "a", "b"}) {
		t.Errorf("script with the root's interpreter: %s %q", path, argv)
	}
	if path, argv := scriptCommand(root, host, []string{"/host.sh"}, vars); path != host || len(argv) != 1 {
		t.Errorf("script with the host's interpreter: %s %q", path, argv)
	}
	if path, _ := scriptCommand(root, binary, []string{"sh"}, vars); path != binary {
		t.Errorf("a binary ran as %s", path)
	}
	path, argv = scriptCommand(root, env, []string{"/env.js", "x"}, vars)
	if path != filepath.Join(root, "bin", "node") || !reflect.DeepEqual(argv, []string{"node", "--stack-size=100", env, "x"}) {
		t.Errorf("script run through env: %s %q", path, argv)
	}
}
