package engine

import (
	"testing"

	"github.com/moby/moby/api/types/container"
)

// Capabilities a container holds already are added and dropped freely;
// one that needs root is refused, an unknown one is invalid.
func TestCapabilities(t *testing.T) {
	for _, c := range []struct {
		add, drop []string
		refused   bool
		kind      Kind
	}{
		{add: []string{"CHOWN"}},
		{add: []string{"CAP_IPC_LOCK"}},
		{add: []string{"ipc_lock"}, drop: []string{"ALL"}},
		{drop: []string{"IPC_LOCK", "NET_RAW"}},
		{add: []string{"SYS_ADMIN"}, refused: true, kind: KindUnsupported},
		{add: []string{"ALL"}, refused: true, kind: KindUnsupported},
		{add: []string{"FLY"}, refused: true, kind: KindInvalid},
		{drop: []string{"FLY"}, refused: true, kind: KindInvalid},
	} {
		err := checkSecurity(&container.HostConfig{CapAdd: c.add, CapDrop: c.drop})
		if !c.refused && err != nil || c.refused && (err == nil || KindOf(err) != c.kind) {
			t.Errorf("add %v drop %v: %v, want kind %v", c.add, c.drop, err, c.kind)
		}
	}
}
