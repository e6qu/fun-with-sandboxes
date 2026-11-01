package forwarder

import (
	"context"

	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/executor"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend/gateway/client"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/session"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/pb"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/worker"
)

func NewGatewayForwarder(w worker.Infos, f client.BuildFunc) frontend.Frontend {
	return &GatewayForwarder{
		workers: w,
		f:       f,
	}
}

type GatewayForwarder struct {
	workers worker.Infos
	f       client.BuildFunc
}

func (gf *GatewayForwarder) Solve(ctx context.Context, llbBridge frontend.FrontendLLBBridge, exec executor.Executor, opts map[string]string, inputs map[string]*pb.Definition, sid string, sm *session.Manager) (retRes *frontend.Result, retErr error) {
	c, err := LLBBridgeToGatewayClient(ctx, llbBridge, exec, opts, inputs, gf.workers, sid, sm)
	if err != nil {
		return nil, err
	}

	defer func() {
		c.discard(retErr)
	}()

	res, err := gf.f(ctx, c)
	if err != nil {
		return nil, err
	}

	return c.toFrontendResult(res)
}
