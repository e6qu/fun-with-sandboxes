//go:build !darwin

package runtime

import (
	"errors"

	"github.com/e6qu/fun-with-sandboxes/internal/roles"
)

// RoleVar names the variable that starts the engine's binary in a helper
// role.
const RoleVar = roles.Var

// Start fails: Seatbelt is macOS's.
func Start(s Spec) (*Process, error) {
	return nil, errors.New("containers run on macOS only")
}
