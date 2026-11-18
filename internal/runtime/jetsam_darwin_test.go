package runtime

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An experiment rather than a check of the engine: what macOS lets an
// unprivileged process do with the jetsam memory limits of itself and its
// children (testdata/jetsam/main.c says each question): whether a limit
// set with posix_spawnattr_setjetsam_ext is enforced, and kept across a
// fork, an exec and a spawn, and what kqueue says of a process killed at
// it. It logs the answers, and fails only when the experiment cannot run.
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
