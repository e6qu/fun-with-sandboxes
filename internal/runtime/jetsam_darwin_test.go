package runtime

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An experiment rather than a check of the engine: whether macOS enforces
// a fatal per-process memory limit that an unprivileged parent sets with
// posix_spawnattr_setjetsam_ext (PLAN.md, roadmap). It logs what happened,
// "killed by signal 9" before 512 MiB if macOS enforces it, and fails only
// when the experiment cannot run.
func TestJetsamLimitExperiment(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "jetsam")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-Werror", "-o", bin, "testdata/jetsam/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin).CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "result: ") {
		t.Fatalf("no result:\n%s", out)
	}
	t.Logf("jetsam experiment:\n%s", out)
}
