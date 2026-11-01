package frontend

import (
	"context"

	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/client/llb/sourceresolver"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/executor"
	gw "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend/gateway/client"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/session"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/pb"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/result"
	digest "github.com/opencontainers/go-digest"
)

const (
	// KeySource is the option key used by the gateway frontend to represent
	// the source for the external frontend
	KeySource = "source"

	KeyDevelDeprecated = "gateway-devel"
)

type Result = result.Result[solver.ResultProxy]

type Attestation = result.Attestation[solver.ResultProxy]

type Frontend interface {
	Solve(ctx context.Context, llb FrontendLLBBridge, exec executor.Executor, opt map[string]string, inputs map[string]*pb.Definition, sid string, sm *session.Manager) (*Result, error)
}

type FrontendLLBBridge interface {
	sourceresolver.MetaResolver
	Solve(ctx context.Context, req SolveRequest, sid string) (*Result, error)
	Warn(ctx context.Context, dgst digest.Digest, msg string, opts WarnOpts) error
}

type SolveRequest = gw.SolveRequest

type CacheOptionsEntry = gw.CacheOptionsEntry

type WarnOpts = gw.WarnOpts
