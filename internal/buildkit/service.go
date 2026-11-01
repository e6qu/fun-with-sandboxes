package buildkit

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"

	ctdmetadata "github.com/containerd/containerd/v2/core/metadata"
	ctdsnapshot "github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/cache/metadata"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/cache/remotecache"
	inlineremotecache "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/cache/remotecache/inline"
	localremotecache "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/cache/remotecache/local"
	registryremotecache "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/cache/remotecache/registry"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/client"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/control"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/exporter"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/exporter/containerimage"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend"
	dockerfile "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend/dockerfile/builder"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend/gateway/forwarder"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/session"
	containerdsnapshot "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/snapshot/containerd"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/bboltcachestorage"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/db/boltutil"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/leaseutil"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/resolver"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/worker"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/worker/base"
	wlabel "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/worker/label"
	"github.com/moby/sys/user"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

type Config struct {
	// Root is where BuildKit keeps its snapshots, content and databases. It
	// lies inside the engine's state root, on the same volume as the image
	// roots so results clone into the image store.
	Root string
	// StateRoot is the engine's state directory, which build steps may not
	// read.
	StateRoot string
	Images    *image.Store
	// Egress returns the loopback port of the egress proxy build steps use.
	Egress func() (int, error)
}

// Service is the embedded BuildKit daemon.
type Service struct {
	controller *control.Controller
	sessions   *session.Manager
	grpc       *grpc.Server
	h2         *http2.Server
	worker     *darwinWorker
}

// darwinWorker adds the engine's moby exporter to BuildKit's base worker.
type darwinWorker struct {
	*base.Worker
	moby exporter.Exporter
}

func (w *darwinWorker) Exporter(name string, sm *session.Manager) (exporter.Exporter, error) {
	if name == ExporterMoby {
		return w.moby, nil
	}
	return w.Worker.Exporter(name, sm)
}

func newWorker(ctx context.Context, cfg Config) (*darwinWorker, error) {
	root := filepath.Join(cfg.Root, "worker")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	sn, err := newSnapshotter(filepath.Join(root, "snapshots"))
	if err != nil {
		return nil, err
	}
	store, err := local.NewStore(filepath.Join(root, "content"))
	if err != nil {
		return nil, err
	}
	db, err := boltutil.Open(filepath.Join(root, "containerdmeta.db"), 0o644, &bolt.Options{FreelistType: bolt.FreelistMapType, NoFreelistSync: true})
	if err != nil {
		return nil, err
	}
	mdb := ctdmetadata.NewDB(db, store, map[string]ctdsnapshot.Snapshotter{SnapshotterName: sn})
	if err := mdb.Init(ctx); err != nil {
		return nil, err
	}
	cs := containerdsnapshot.NewContentStore(mdb.ContentStore(), namespace)
	md, err := metadata.NewStore(filepath.Join(root, "metadata_v2.db"))
	if err != nil {
		return nil, err
	}
	id, err := base.ID(root)
	if err != nil {
		return nil, err
	}
	exe, err := newExecutor(execOpt{StateRoot: cfg.StateRoot, ProfileDir: filepath.Join(root, "executor"), Egress: cfg.Egress})
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	lm := leaseutil.WithNamespace(ctdmetadata.NewLeaseManager(mdb), namespace)
	// Build steps own their files as root (uid and gid 0), and BuildKit
	// chowns accordingly. Mapping 0 to the engine's user, as rootless
	// BuildKit maps it, makes those chowns ones an unprivileged process may
	// make.
	idmap := &user.IdentityMapping{
		UIDMaps: []user.IDMap{{ID: 0, ParentID: int64(os.Getuid()), Count: 1}},
		GIDMaps: []user.IDMap{{ID: 0, ParentID: int64(os.Getgid()), Count: 1}},
	}
	opt := base.WorkerOpt{
		ID:   id,
		Root: root,
		Labels: map[string]string{
			wlabel.Executor:    "seatbelt",
			wlabel.Snapshotter: SnapshotterName,
			wlabel.Hostname:    hostname,
			wlabel.Network:     "egress-proxy",
		},
		MetadataStore:   md,
		ContentMetadata: db,
		Executor:        exe,
		Snapshotter:     containerdsnapshot.NewSnapshotter(SnapshotterName, mdb.Snapshotter(SnapshotterName), namespace, idmap),
		IdentityMapping: idmap,
		ContentStore:    cs,
		Applier:         applier{store: cs},
		Differ:          comparer{store: cs},
		ImageStore: &localImages{
			engine: cfg.Images, meta: ctdmetadata.NewImageStore(mdb), content: cs, leases: lm,
		},
		Platforms:      []ocispecs.Platform{{OS: "darwin", Architecture: goruntime.GOARCH}},
		RegistryHosts:  resolver.NewRegistryConfig(nil),
		LeaseManager:   lm,
		GarbageCollect: mdb.GarbageCollect,
		MountPoolRoot:  filepath.Join(root, "cachemounts"),
	}
	w, err := base.NewWorker(ctx, opt)
	if err != nil {
		return nil, err
	}
	writer, err := containerimage.NewImageWriter(containerimage.WriterOpt{
		Snapshotter: opt.Snapshotter, ContentStore: opt.ContentStore, Applier: opt.Applier, Differ: opt.Differ,
	})
	if err != nil {
		return nil, err
	}
	return &darwinWorker{Worker: w, moby: &mobyExporter{store: cfg.Images, writer: writer, content: opt.ContentStore, leases: lm}}, nil
}

// New starts BuildKit's controller on the darwin worker.
func New(ctx context.Context, cfg Config) (*Service, error) {
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, err
	}
	sm, err := session.NewManager()
	if err != nil {
		return nil, err
	}
	w, err := newWorker(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("buildkit worker: %w", err)
	}
	wc := &worker.Controller{}
	if err := wc.Add(w); err != nil {
		return nil, err
	}
	cacheStorage, err := bboltcachestorage.NewStore(filepath.Join(cfg.Root, "cache.db"))
	if err != nil {
		return nil, err
	}
	historyDB, err := boltutil.Open(filepath.Join(cfg.Root, "history.db"), 0o600, &bolt.Options{FreelistType: bolt.FreelistMapType, NoFreelistSync: true})
	if err != nil {
		return nil, err
	}
	hosts := resolver.NewRegistryConfig(nil)
	ctl, err := control.NewController(control.Opt{
		SessionManager:   sm,
		WorkerController: wc,
		Frontends: map[string]frontend.Frontend{
			"dockerfile.v0": forwarder.NewGatewayForwarder(wc.Infos(), dockerfile.Build),
		},
		ResolveCacheExporterFuncs: map[string]remotecache.ResolveCacheExporterFunc{
			"registry": registryremotecache.ResolveCacheExporterFunc(sm, hosts),
			"local":    localremotecache.ResolveCacheExporterFunc(sm),
			"inline":   inlineremotecache.ResolveCacheExporterFunc(),
		},
		ResolveCacheImporterFuncs: map[string]remotecache.ResolveCacheImporterFunc{
			"registry": registryremotecache.ResolveCacheImporterFunc(sm, w.ContentStore(), hosts),
			"local":    localremotecache.ResolveCacheImporterFunc(sm),
		},
		CacheManager:   solver.NewCacheManager(ctx, "local", cacheStorage, worker.NewCacheResultStorage(wc)),
		HistoryDB:      historyDB,
		CacheStore:     cacheStorage,
		LeaseManager:   w.LeaseManager(),
		ContentStore:   w.ContentStore(),
		GarbageCollect: w.GarbageCollect,
	})
	if err != nil {
		return nil, err
	}
	srv := grpc.NewServer()
	ctl.Register(srv)
	return &Service{controller: ctl, sessions: sm, grpc: srv, h2: &http2.Server{}, worker: w}, nil
}

// ServeGRPC upgrades the request to HTTP/2 and serves BuildKit's control API
// on it, as dockerd serves POST /grpc.
func (s *Service) ServeGRPC(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection cannot be hijacked", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	resp := &http.Response{StatusCode: http.StatusSwitchingProtocols, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}}
	resp.Header.Set("Connection", "Upgrade")
	resp.Header.Set("Upgrade", "h2c")
	if err := resp.Write(conn); err != nil {
		conn.Close()
		return
	}
	s.h2.ServeConn(conn, &http2.ServeConnOpts{Handler: s.grpc})
}

// ServeSession attaches a client's session, through which BuildKit reads the
// build context and secrets and writes local outputs.
func (s *Service) ServeSession(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.HandleHTTPRequest(r.Context(), w, r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func (s *Service) Close() error {
	s.grpc.Stop()
	err := s.controller.Close()
	if werr := s.worker.Close(); err == nil {
		err = werr
	}
	return err
}

// Prune removes BuildKit's build cache, records in use excepted, all of it
// with all set and otherwise what BuildKit's default policy deems
// reclaimable, and returns the IDs and bytes it freed.
func (s *Service) Prune(ctx context.Context, all bool) ([]string, uint64, error) {
	ch := make(chan client.UsageInfo)
	var ids []string
	var freed uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for u := range ch {
			ids = append(ids, u.ID)
			freed += uint64(u.Size)
		}
	}()
	err := s.worker.Prune(ctx, ch, client.PruneInfo{All: all})
	close(ch)
	<-done
	return ids, freed, err
}
