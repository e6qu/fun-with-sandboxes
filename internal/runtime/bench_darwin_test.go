package runtime

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/pathmap"
	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
)

// BenchmarkStart measures how long Start takes to have a container's main
// process running: the spawn helper, its limits, its sandbox, the exec
// anchor it starts and the container library the command loads.
func BenchmarkStart(b *testing.B) {
	state, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	root := filepath.Join(state, "root")
	if out, err := exec.Command("go", "build", "-o", filepath.Join(root, "signal"), "./testdata/signal").CombinedOutput(); err != nil {
		b.Fatalf("build signal: %v\n%s", err, out)
	}
	lib, err := pathmap.Build(filepath.Join(state, "lib"))
	if err != nil {
		b.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "fws")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { os.RemoveAll(dir) })
	spec := func() Spec {
		return Spec{
			Root: root, Argv: []string{"/signal"}, Env: []string{"PATH=/"},
			Profile:     seatbelt.Spec{Root: root, StateRoot: state, IPCPrefix: seatbelt.IPCPrefixFor(root)},
			ProfilePath: filepath.Join(state, "profile.sb"),
		}
	}
	for _, mode := range []struct {
		name    string
		library bool
		anchor  bool
	}{
		{"sandbox", false, false},
		{"library", true, false},
		{"anchor", false, true},
		{"container", true, true},
	} {
		b.Run(mode.name, func(b *testing.B) {
			var total time.Duration
			for i := 0; i < b.N; i++ {
				s := spec()
				if mode.library {
					s.Library = lib
					s.Profile.Libraries = []string{lib}
				}
				var lf *os.File
				if mode.anchor {
					sock := filepath.Join(dir, "anchor")
					os.Remove(sock)
					l, err := net.Listen("unix", sock)
					if err != nil {
						b.Fatal(err)
					}
					ul := l.(*net.UnixListener)
					ul.SetUnlinkOnClose(false)
					if lf, err = ul.File(); err != nil {
						b.Fatal(err)
					}
					ul.Close()
					s.AnchorListener = lf
				}
				start := time.Now()
				p, err := Start(s)
				total += time.Since(start)
				if lf != nil {
					lf.Close()
				}
				if err != nil {
					b.Fatal(err)
				}
				p.KillAll()
				<-p.Done()
			}
			b.ReportMetric(float64(total.Nanoseconds())/float64(b.N), "ns/start")
		})
	}
}
