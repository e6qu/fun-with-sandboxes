package worker

import (
	"context"
	"io"

	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/cache"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/client"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/client/llb/sourceresolver"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/executor"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/exporter"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/session"
	containerdsnapshot "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/snapshot/containerd"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/llbsolver/cdidevices"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/pb"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/source"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/leaseutil"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/network"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

type ProxyOpt struct {
	Network bool
	Policy  func() (network.ProxyPolicy, error)
}

type Worker interface {
	io.Closer
	// ID needs to be unique in the cluster
	ID() string
	Labels() map[string]string
	Platforms(noCache bool) []ocispecs.Platform
	BuildkitVersion() client.BuildkitVersion

	GCPolicy() []client.PruneInfo
	LoadRef(ctx context.Context, id string, hidden bool) (cache.ImmutableRef, error)
	// ResolveOp resolves Vertex.Sys() to Op implementation.
	ResolveOp(v solver.Vertex, s frontend.FrontendLLBBridge, sm *session.Manager, proxyOpt ProxyOpt) (solver.Op, error)
	ParseSource(op *pb.SourceOp, platform *pb.Platform) (source.Identifier, error)
	ResolveSourceMetadata(ctx context.Context, op *pb.SourceOp, opt sourceresolver.Opt, sm *session.Manager, jobCtx solver.JobContext) (*sourceresolver.MetaResponse, error)
	DiskUsage(ctx context.Context, opt client.DiskUsageInfo) ([]*client.UsageInfo, error)
	Exporter(name string, sm *session.Manager) (exporter.Exporter, error)
	Prune(ctx context.Context, ch chan client.UsageInfo, opt ...client.PruneInfo) error
	FromRemote(ctx context.Context, remote *solver.Remote) (cache.ImmutableRef, error)
	PruneCacheMounts(ctx context.Context, ids map[string]bool) error
	ContentStore() *containerdsnapshot.Store
	Executor() executor.Executor
	CacheManager() cache.Manager
	LeaseManager() *leaseutil.Manager
	GarbageCollect(context.Context) error
	CDIManager() *cdidevices.Manager
}

type Infos interface {
	DefaultCacheManager() (cache.Manager, error)
	WorkerInfos() []client.WorkerInfo
}
