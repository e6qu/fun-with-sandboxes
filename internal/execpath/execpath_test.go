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
