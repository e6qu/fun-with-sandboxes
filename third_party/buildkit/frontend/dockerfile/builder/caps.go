package builder

import (
	"strings"

	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/errdefs"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/grpcerrors"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/stack"
	"google.golang.org/grpc/codes"
)

var enabledCaps = map[string]struct{}{
	"moby.buildkit.frontend.inputs":         {},
	"moby.buildkit.frontend.subrequests":    {},
	"moby.buildkit.frontend.contexts":       {},
	"moby.buildkit.frontend.gitquerystring": {},
	"moby.buildkit.frontend.contexts.zstd":  {},
}

func validateCaps(req string) (forward bool, err error) {
	if req == "" {
		return
	}
	caps := strings.SplitSeq(req, ",")
	for c := range caps {
		parts := strings.SplitN(c, "+", 2)
		if _, ok := enabledCaps[parts[0]]; !ok {
			err = stack.Enable(grpcerrors.WrapCode(errdefs.NewUnsupportedFrontendCapError(parts[0]), codes.Unimplemented))
			if strings.Contains(c, "+forward") {
				forward = true
			} else {
				return false, err
			}
		}
	}
	return
}
