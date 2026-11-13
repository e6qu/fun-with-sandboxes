package runtime

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
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
		// cold starts each time without the compiled profile, as a
		// container's first start the engine did not compile ahead.
		cold bool
	}{
		{"sandbox", false, false, false},
		{"library", true, false, false},
		{"anchor", false, true, false},
		{"container", true, true, false},
		{"container-cold", true, true, true},
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
				if mode.cold {
					os.Remove(compiledPath(s))
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

// BenchmarkSpawn measures a C program's run from spawn to exit: natively,
// under the container's Seatbelt profile through sandbox-exec, a C binary
// that compiles the profile and executes the program, and through Start's
// spawn helper, which also applies the limits.
func BenchmarkSpawn(b *testing.B) {
	state, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	root := filepath.Join(state, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		b.Fatal(err)
	}
	src := filepath.Join(state, "true.c")
	if err := os.WriteFile(src, []byte("int main(void) { return 0; }\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	if out, err := exec.Command("cc", "-O2", "-o", filepath.Join(root, "true"), src).CombinedOutput(); err != nil {
		b.Fatalf("cc: %v\n%s", err, out)
	}
	profile := seatbelt.Spec{Root: root, StateRoot: state, IPCPrefix: seatbelt.IPCPrefixFor(root)}
	text, err := seatbelt.Render(profile)
	if err != nil {
		b.Fatal(err)
	}
	profilePath := filepath.Join(state, "profile.sb")
	if err := os.WriteFile(profilePath, []byte(text), 0o600); err != nil {
		b.Fatal(err)
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer null.Close()
	run := func(b *testing.B, argv []string) {
		for i := 0; i < b.N; i++ {
			pid, err := libsystem.Spawn(argv[0], argv, nil, libsystem.SpawnAttr{Dir: root, Files: []uintptr{null.Fd(), null.Fd(), null.Fd()}})
			if err != nil {
				b.Fatal(err)
			}
			var ws syscall.WaitStatus
			if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil || ws.ExitStatus() != 0 {
				b.Fatalf("%v: %v, %v", argv, ws, err)
			}
		}
	}
	b.Run("native", func(b *testing.B) { run(b, []string{filepath.Join(root, "true")}) })
	b.Run("sandbox-exec", func(b *testing.B) {
		run(b, []string{"/usr/bin/sandbox-exec", "-f", profilePath, filepath.Join(root, "true")})
	})
	b.Run("helper", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			p, err := Start(Spec{
				Root: root, Argv: []string{"/true"}, Env: []string{"PATH=/"},
				Profile: profile, ProfilePath: profilePath,
			})
			if err != nil {
				b.Fatal(err)
			}
			<-p.Done()
		}
	})
}
