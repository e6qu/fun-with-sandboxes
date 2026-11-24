package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/dockerfile"
	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// ENTRYPOINT drops the CMD a stage inherits, and keeps one the stage set,
// as the classic builder does.
func TestEntrypointKeepsTheStagesOwnCmd(t *testing.T) {
	base := image.Config{ConfigFile: v1.ConfigFile{Config: v1.Config{Cmd: []string{"inherited"}}}}
	e := &Engine{}
	got, err := e.applyChanges(context.Background(), base, []string{`ENTRYPOINT ["run"]`})
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.Cmd != nil {
		t.Errorf("an inherited CMD survived ENTRYPOINT: %q", got.Config.Cmd)
	}
	got, err = e.applyChanges(context.Background(), base, []string{`CMD ["own"]`, `ENTRYPOINT ["run"]`})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Config.Cmd, []string{"own"}) || !slices.Equal(got.Config.Entrypoint, []string{"run"}) {
		t.Errorf("CMD then ENTRYPOINT: cmd %q, entrypoint %q", got.Config.Cmd, got.Config.Entrypoint)
	}
}

// An ARG line may declare several arguments, each taking its build
// argument, else its default, as Docker reads it.
func TestArgsOnOneLine(t *testing.T) {
	over := "from-cli"
	b := &builder{opts: BuildOptions{BuildArgs: map[string]*string{"b": &over}}}
	ins, err := dockerfile.Parse(strings.NewReader("ARG a=1 b=2 c\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.args(ins[0], func(k string) (string, bool) { return "looked-up-" + k, k == "c" })
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"a", "1"}, {"b", "from-cli"}, {"c", "looked-up-c"}}
	if !slices.Equal(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
}

// A COPY wildcard that stands for a context's link to a host directory
// matches what the link's target holds in the context, not on the host.
func TestGlobInStaysInRoot(t *testing.T) {
	root, host := t.TempDir(), t.TempDir()
	for _, p := range []string{filepath.Join(host, "secret"), filepath.Join(root, "d", "secret")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(host, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/d", filepath.Join(root, "inroot")); err != nil {
		t.Fatal(err)
	}
	got, err := globIn(root, filepath.Join(root, "*", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "d", "secret")}
	if !slices.Equal(got, want) {
		t.Errorf("globIn = %q, want %q", got, want)
	}
	if got, err := globIn(root, root); err != nil || !slices.Equal(got, []string{root}) {
		t.Errorf("globIn of the root = %q, %v", got, err)
	}
}
