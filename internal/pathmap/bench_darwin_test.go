package pathmap

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	// one and few interpose one rarely bound function, and four bound
	// nearly everywhere: whether the loader's cost of fwsroot's interposing
	// grows with what it interposes, or with how widely that is bound.
	interposers := map[string]string{}
	for _, name := range []string{"one", "few"} {
		lib := filepath.Join(b.TempDir(), name+".dylib")
		if out, err := exec.Command("/usr/bin/xcrun", "clang", "-dynamiclib", "-O2", "-o", lib, "testdata/interpose/"+name+".c").CombinedOutput(); err != nil {
			b.Fatalf("build the %s library: %v\n%s", name, err, out)
		}
		interposers[name] = lib
	}
	// many interposes every function fwsroot does, each with a stub that
	// only jumps to it: the loader's cost of fwsroot's interposing alone,
	// apart from what its initializer does.
	syms := interposed(b)
	interposers["many"] = interposer(b, "many", syms)
	for _, op := range []string{"getpid", "getuid", "open", "stat", "stat-missing", "fstat", "pread", "readdir", "socket", "spawn", "started"} {
		// empty inserts a library that does nothing: what the loader costs.
		// bare inserts fwsroot without a container's variables: what its
		// interposing costs the loader, beside what it does for a container.
		modes := []string{"native", "empty", "bare", "fwsroot"}
		if op == "spawn" || op == "started" {
			modes = append(modes, "one", "few", "many")
		}
		for _, mode := range modes {
			b.Run(op+"/"+mode, func(b *testing.B) {
				dir, env := root, []string(nil)
				switch mode {
				case "native":
				case "empty":
					env = []string{"DYLD_INSERT_LIBRARIES=" + empty}
				case "bare":
					env = []string{"DYLD_INSERT_LIBRARIES=" + lib}
				case "fwsroot":
					dir, env = "", []string{"DYLD_INSERT_LIBRARIES=" + lib, "SANDBOX_ROOT=" + root}
				default:
					env = []string{"DYLD_INSERT_LIBRARIES=" + interposers[mode]}
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

// interposed lists the symbols fwsroot interposes, as its INTERPOSE lines
// name them, with the names its __asm labels give.
func interposed(tb testing.TB) []string {
	src, err := os.ReadFile("src/fwsroot.c")
	if err != nil {
		tb.Fatal(err)
	}
	labels := map[string]string{}
	for _, m := range regexp.MustCompile(`(\w+)\([^;]*\)\s*__asm\("_([^"]+)"\)`).FindAllStringSubmatch(string(src), -1) {
		labels[m[1]] = m[2]
	}
	var syms []string
	for _, m := range regexp.MustCompile(`(?m)^INTERPOSE\(\w+,\s*(\w+)\);`).FindAllStringSubmatch(string(src), -1) {
		name := m[1]
		if l, ok := labels[name]; ok {
			name = l
		}
		syms = append(syms, name)
	}
	if len(syms) < 100 {
		tb.Fatalf("found %d interposed functions in fwsroot.c", len(syms))
	}
	return syms
}

// interposer builds a library that interposes each of syms with a stub
// that jumps to it, and returns its path.
func interposer(tb testing.TB, name string, syms []string) string {
	var asm strings.Builder
	asm.WriteString(".text\n")
	for i, s := range syms {
		fmt.Fprintf(&asm, ".weak_reference \"_%s\"\n.p2align 2\nstub_%d:\n\tb \"_%s\"\n", s, i, s)
	}
	asm.WriteString(".section __DATA,__interpose\n.p2align 3\n")
	for i, s := range syms {
		fmt.Fprintf(&asm, ".quad stub_%d\n.quad \"_%s\"\n", i, s)
	}
	dir := tb.TempDir()
	src, lib := filepath.Join(dir, name+".s"), filepath.Join(dir, name+".dylib")
	if err := os.WriteFile(src, []byte(asm.String()), 0o644); err != nil {
		tb.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-dynamiclib", "-Wl,-undefined,dynamic_lookup", "-o", lib, src).CombinedOutput(); err != nil {
		tb.Fatalf("build the %s library: %v\n%s", name, err, out)
	}
	tb.Logf("%s interposes %s", name, strings.Join(syms, " "))
	return lib
}

// BenchmarkStartCost times how long a process takes to reach main under
// each library, and under libraries that interpose each eighth of
// fwsroot's functions, in fwsroot.c's order: what each part of fwsroot's
// interposing costs a container's every process start. A CI runner's
// speed drifts more than these differ, so each round starts each case once,
// in a shuffled order, and each case's median over the rounds is printed.
func BenchmarkStartCost(b *testing.B) {
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
	syms := interposed(b)
	type mode struct {
		name string
		env  []string
	}
	modes := []mode{
		{"native", nil},
		{"empty", []string{"DYLD_INSERT_LIBRARIES=" + empty}},
		{"many", []string{"DYLD_INSERT_LIBRARIES=" + interposer(b, "many", syms)}},
		{"fwsroot", []string{"DYLD_INSERT_LIBRARIES=" + lib, "SANDBOX_ROOT=" + root}},
	}
	const groups = 8
	for g := range groups {
		name := fmt.Sprintf("many-%d", g)
		part := syms[g*len(syms)/groups : (g+1)*len(syms)/groups]
		modes = append(modes, mode{name, []string{"DYLD_INSERT_LIBRARIES=" + interposer(b, name, part)}})
		fmt.Printf("%s: %s\n", name, strings.Join(part, " "))
	}
	const rounds = 25
	times := map[string][]float64{}
	for range b.N {
		for range rounds {
			order := slices.Clone(modes)
			rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			for _, m := range order {
				cmd := exec.Command(bin, "started", "1", root)
				cmd.Env = m.env
				out, err := cmd.CombinedOutput()
				if err != nil {
					b.Fatalf("%s: %v\n%s", m.name, err, out)
				}
				ns, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
				if err != nil {
					b.Fatalf("%s printed %q", m.name, out)
				}
				times[m.name] = append(times[m.name], ns)
			}
		}
	}
	for _, m := range modes {
		t := slices.Clone(times[m.name])
		slices.Sort(t)
		fmt.Printf("start cost %-8s median %6.2f ms, quartiles %6.2f-%6.2f ms\n", m.name, t[len(t)/2]/1e6, t[len(t)/4]/1e6, t[3*len(t)/4]/1e6)
	}
}
