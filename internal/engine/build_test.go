package engine

import (
	"context"
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
