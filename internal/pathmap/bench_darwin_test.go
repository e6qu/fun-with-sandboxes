package pathmap

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// BenchmarkCalls times system calls a container's processes make: natively,
// with an empty library inserted, and through fwsroot, which a container's
// processes pay on every call it interposes. Each reports the nanoseconds
// one call took.
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
	empty := filepath.Join(b.TempDir(), "empty.dylib")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-dynamiclib", "-o", empty, "testdata/empty/empty.c").CombinedOutput(); err != nil {
		b.Fatalf("build the empty library: %v\n%s", err, out)
	}
	for _, op := range []string{"getpid", "getuid", "open", "stat", "stat-missing", "fstat", "pread", "readdir", "socket", "spawn", "started"} {
		// empty inserts a library that does nothing: what the loader costs.
		// bare inserts fwsroot without a container's variables: what its
		// interposing costs the loader, beside what it does for a container.
		for _, mode := range []string{"native", "empty", "bare", "fwsroot"} {
			b.Run(op+"/"+mode, func(b *testing.B) {
				dir, env := root, []string(nil)
				switch mode {
				case "empty":
					env = []string{"DYLD_INSERT_LIBRARIES=" + empty}
				case "bare":
					env = []string{"DYLD_INSERT_LIBRARIES=" + lib}
				case "fwsroot":
					dir, env = "", []string{"DYLD_INSERT_LIBRARIES=" + lib, "SANDBOX_ROOT=" + root}
				}
				run := func(n int) float64 {
					cmd := exec.Command(bin, op, strconv.Itoa(n), dir)
					cmd.Env = env
					out, err := cmd.CombinedOutput()
					if err != nil {
						b.Fatalf("%s: %v\n%s", op, err, out)
					}
					ns, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
					if err != nil {
						b.Fatalf("%s printed %q", op, out)
					}
					return ns
				}
				// started times one process start, so each iteration starts one.
				if op == "started" {
					var sum float64
					for range b.N {
						sum += run(1)
					}
					b.ReportMetric(sum/float64(b.N), "ns/call")
					return
				}
				b.ReportMetric(run(b.N), "ns/call")
			})
		}
	}
}
