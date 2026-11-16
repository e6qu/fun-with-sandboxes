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

// seatbelt=gui is the one grant --security-opt names; any other is refused.
func TestSeatbeltSecurityOptions(t *testing.T) {
	for opt, ok := range map[string]bool{"seatbelt=gui": true, "seatbelt:gui": true, "seatbelt=all": false, "seatbelt": false} {
		hc := &container.HostConfig{SecurityOpt: []string{opt}}
		if err := checkSecurity(hc); ok != (err == nil) {
			t.Errorf("--security-opt %s: %v", opt, err)
		}
		if guiGranted(hc) != ok {
			t.Errorf("--security-opt %s: granted %v", opt, guiGranted(hc))
		}
	}
}
