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

// A PATH read inside a container and handed back names the same
// directories as the container's own, and the engine's variables are its
// own to set.
func TestMapPathIsIdempotent(t *testing.T) {
	const root = "/state/c/1/r"
	once := mapPath(root, "/usr/bin:/bin:relative")
	if once != root+"/usr/bin:"+root+"/bin" {
		t.Fatalf("mapped PATH: %s", once)
	}
	if again := mapPath(root, once+":/opt/bin"); again != once+":"+root+"/opt/bin" {
		t.Errorf("a mapped PATH mapped again: %s", again)
	}
	if other := mapPath(root, "/state/c/1/rest"); other != root+"/state/c/1/rest" {
		t.Errorf("a directory that only starts like the root: %s", other)
	}
	for k, want := range map[string]bool{"FWS_UID": true, "DYLD_INSERT_LIBRARIES": true, "PATH": false, "SANDBOX_ROOT": false} {
		if engineVariable(k) != want {
			t.Errorf("engineVariable(%s) = %v", k, !want)
		}
	}
}
