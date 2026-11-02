//go:build !darwin

package runtime

import "errors"

// RoleVar names the variable that starts the engine's binary in a helper
// role.
const RoleVar = "FWS_ROLE"

// Start fails: Seatbelt and sandbox-exec are macOS's.
func Start(s Spec) (*Process, error) {
	return nil, errors.New("containers run on macOS only")
}
