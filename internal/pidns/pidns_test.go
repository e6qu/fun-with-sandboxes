package pidns

import (
	"path/filepath"
	"slices"
	"testing"
)

// A namespace's file lists its owner first and its joiners in the order
// they joined; one leaving takes itself out alone.
func TestJoinAndLeave(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ns")
	if err := Write(file, []string{"SANDBOX_ROOT=/a"}); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"SANDBOX_ROOT=/b", "SANDBOX_ROOT=/c", "SANDBOX_ROOT=/b"} {
		if err := Join(file, tag); err != nil {
			t.Fatal(err)
		}
	}
	if tags, err := Tags(file); err != nil || !slices.Equal(tags, []string{"SANDBOX_ROOT=/a", "SANDBOX_ROOT=/b", "SANDBOX_ROOT=/c"}) {
		t.Fatalf("after joins: %q, %v", tags, err)
	}
	if err := Leave(file, "SANDBOX_ROOT=/b"); err != nil {
		t.Fatal(err)
	}
	if tags, err := Tags(file); err != nil || !slices.Equal(tags, []string{"SANDBOX_ROOT=/a", "SANDBOX_ROOT=/c"}) {
		t.Fatalf("after a leave: %q, %v", tags, err)
	}
	if err := Leave(filepath.Join(t.TempDir(), "gone"), "SANDBOX_ROOT=/b"); err != nil {
		t.Errorf("leaving an ended namespace: %v", err)
	}
	if err := Write(file, []string{"SANDBOX_ROOT=/a\nSANDBOX_ROOT=/x"}); err == nil {
		t.Error("a tag holding a newline was written")
	}
}
