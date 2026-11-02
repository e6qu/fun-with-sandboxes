//go:build !darwin

package shim

import (
	"encoding/json"
	"os"

	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
)

// A shim on another system reports that containers run on macOS only.
func init() {
	if os.Getenv(runtime.RoleVar) == Role && len(os.Args) > 1 {
		ready := os.NewFile(3, "ready")
		json.NewEncoder(ready).Encode(Ready{Error: "containers run on macOS only"})
		os.Exit(1)
	}
}
