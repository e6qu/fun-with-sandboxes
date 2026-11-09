package pathmap

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// BenchmarkCalls times system calls a container's processes make, natively
// and through the library, which a container's processes pay on every
// call it interposes. Each reports the nanoseconds one call took.
func BenchmarkCalls(b *testing.B) {
	root := containerRoot(b)
	bin := filepath.Join(root, "bin", "bench")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-O2", "-Wall", "-Werror", "-o", bin, "testdata/bench/main.c").CombinedOutput(); err != nil {
		b.Fatalf("build the benchmark: %v\n%s", err, out)
	}
	lib, err := Build(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for _, op := range []string{"getpid", "getuid", "open", "stat", "stat-missing", "fstat", "pread", "readdir", "socket", "spawn"} {
		for _, mode := range []string{"native", "fwsroot"} {
			b.Run(op+"/"+mode, func(b *testing.B) {
				dir, env := root, []string(nil)
				if mode == "fwsroot" {
					dir, env = "", []string{"DYLD_INSERT_LIBRARIES=" + lib, "SANDBOX_ROOT=" + root}
				}
				cmd := exec.Command(bin, op, strconv.Itoa(b.N), dir)
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				if err != nil {
					b.Fatalf("%s: %v\n%s", op, err, out)
				}
				ns, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
				if err != nil {
					b.Fatalf("%s printed %q", op, out)
				}
				b.ReportMetric(ns, "ns/call")
			})
		}
	}
}
