package execpath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "tool"), []byte("#"), 0o755)
	os.WriteFile(filepath.Join(bin, "data"), []byte("#"), 0o644)
	want := filepath.Join(bin, "tool")
	for name, c := range map[string]struct {
		argv0 string
		env   []string
	}{
		"absolute":            {"/bin/tool", nil},
		"container PATH":      {"tool", []string{"PATH=/usr/bin:/bin"}},
		"PATH already mapped": {"tool", []string{"PATH=" + bin}},
	} {
		got, err := Resolve(root, c.argv0, c.env)
		if err != nil || got != want {
			t.Errorf("%s: Resolve(%q) = %q, %v; want %q", name, c.argv0, got, err, want)
		}
	}
	for _, argv0 := range []string{"data", "/bin/data", "missing", "/../../bin/tool-elsewhere", ""} {
		if got, err := Resolve(root, argv0, []string{"PATH=/bin"}); err == nil {
			t.Errorf("Resolve(%q) = %q, want error", argv0, got)
		}
	}
}

func TestFromWorkDir(t *testing.T) {
	for _, c := range [][3]string{
		{"/qdrant", "./entrypoint.sh", "/qdrant/entrypoint.sh"},
		{"", "bin/tool", "/bin/tool"},
		{"/app", "../bin/tool", "/bin/tool"},
		{"/app", "/bin/tool", "/bin/tool"},
		{"/app", "tool", "tool"},
	} {
		if got := FromWorkDir(c[0], c[1]); got != c[2] {
			t.Errorf("FromWorkDir(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

// A program reached through the container's own links, one onto a mount,
// resolves as the container sees it: an absolute link from the container's
// root, a mount's destination in its source, never the host's paths.
func TestResolveThroughLinksAndMounts(t *testing.T) {
	root, vol := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(vol, "server", "bin"), 0o755)
	os.WriteFile(filepath.Join(vol, "server", "bin", "code-server"), []byte("#"), 0o755)
	os.MkdirAll(filepath.Join(root, "root", ".server", "bin"), 0o755)
	// The extension's link, absolute in the container, onto the volume.
	os.Symlink("/vscode/server", filepath.Join(root, "root", ".server", "bin", "c1"))
	// The engine's link for the mount, which names the volume on the host.
	os.Symlink(vol, filepath.Join(root, "vscode"))
	// A relative link in the root, and one that leaves the root by ..
	os.MkdirAll(filepath.Join(root, "opt"), 0o755)
	os.WriteFile(filepath.Join(root, "opt", "tool"), []byte("#"), 0o755)
	os.Symlink("../opt", filepath.Join(root, "usr-opt"))
	os.Symlink("../../../../../opt", filepath.Join(root, "deep"))
	os.Symlink("loop", filepath.Join(root, "loop"))
	env := []string{"FWS_MOUNTS=/vscode\t" + vol + "\n/other\t/nowhere", "PATH=/usr-opt"}
	for argv0, want := range map[string]string{
		"/root/.server/bin/c1/bin/code-server": filepath.Join(vol, "server", "bin", "code-server"),
		"/vscode/server/bin/code-server":       filepath.Join(vol, "server", "bin", "code-server"),
		"/usr-opt/tool":                        filepath.Join(root, "opt", "tool"),
		"/deep/tool":                           filepath.Join(root, "opt", "tool"),
		"tool":                                 filepath.Join(root, "opt", "tool"),
	} {
		if got, err := Resolve(root, argv0, env); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", argv0, got, err, want)
		}
	}
	for _, argv0 := range []string{"/loop/tool", "/root/.server/bin/c1/bin/missing"} {
		if got, err := Resolve(root, argv0, env); err == nil {
			t.Errorf("Resolve(%q) = %q, want error", argv0, got)
		}
	}
}

// A working directory reached through a link onto a mount is the mount's,
// and one not made yet is kept, to be made.
func TestHostPath(t *testing.T) {
	root, vol := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(vol, "server"), 0o755)
	os.MkdirAll(filepath.Join(root, "root", "bin"), 0o755)
	os.Symlink("/vscode/server", filepath.Join(root, "root", "bin", "c1"))
	os.Symlink(vol, filepath.Join(root, "vscode"))
	env := []string{"FWS_MOUNTS=/vscode\t" + vol}
	for p, want := range map[string]string{
		"/root/bin/c1":         filepath.Join(vol, "server"),
		"/root/bin/c1/new/dir": filepath.Join(vol, "server", "new", "dir"),
		"/work/new":            filepath.Join(root, "work", "new"),
		"":                     root,
	} {
		if got := HostPath(root, p, env); got != want {
			t.Errorf("HostPath(%q) = %q, want %q", p, got, want)
		}
	}
}
