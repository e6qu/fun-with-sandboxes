package engine

import (
	"context"
	"slices"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"

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
