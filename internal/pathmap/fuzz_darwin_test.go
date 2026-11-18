package pathmap

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The path mapping's fuzzer (testdata/fuzz) maps random paths through the
// library's own code, under AddressSanitizer and UndefinedBehaviorSanitizer,
// for FWS_PATH_FUZZ_SECONDS, 2 by default, from FWS_PATH_FUZZ_SEED or a
// seed it prints. Apple's clang has no libFuzzer; the fuzzer makes its
// paths from the parts that trip a mapping up.
func TestPathMappingFuzz(t *testing.T) {
	secs := os.Getenv("FWS_PATH_FUZZ_SECONDS")
	if secs == "" {
		secs = "2"
	}
	bin := filepath.Join(t.TempDir(), "fuzz")
	args := append([]string{"clang", "-g", "-O1", "-Wall", "-fsanitize=address,undefined", "-fno-sanitize-recover=undefined"}, weakSymbols...)
	if out, err := exec.Command("/usr/bin/xcrun", append(args, "-o", bin, "testdata/fuzz/main.c")...).CombinedOutput(); err != nil {
		t.Fatalf("build the fuzzer: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, secs)
	if seed := os.Getenv("FWS_PATH_FUZZ_SEED"); seed != "" {
		cmd.Args = append(cmd.Args, seed)
	}
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the fuzzer: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
