package dockerfile2llb

import (
	"github.com/pkg/errors"

	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/client/llb"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend/dockerfile/instructions"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/pb"
)

func dispatchRunSecurity(c *instructions.RunCommand) (llb.RunOption, error) {
	security := instructions.GetSecurity(c)

	switch security {
	case instructions.SecurityInsecure:
		return llb.Security(pb.SecurityMode_INSECURE), nil
	case instructions.SecuritySandbox:
		return llb.Security(pb.SecurityMode_SANDBOX), nil
	default:
		return nil, errors.Errorf("unsupported security mode %q", security)
	}
}
