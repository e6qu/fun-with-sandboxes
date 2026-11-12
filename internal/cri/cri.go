// Package cri serves the Kubernetes Container Runtime Interface over the
// engine, as cri-dockerd serves it over Docker's: a pod sandbox is a pause
// container that holds the pod's network, its ports and its DNS, and the
// pod's containers join that network as --network container: joins one.
// The kubelet reads each container's output from the log file the
// runtime writes where the kubelet asks, in CRI's format.
package cri

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/cri-streaming/pkg/streaming"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// DefaultSandboxImage is the pause image a sandbox runs, the one the
// kubelet of Kubernetes 1.35 names.
const DefaultSandboxImage = "fws/pause:3.10.1"

// Labels the runtime keeps a CRI object's identity in, on the engine's
// container that holds it.
const (
	kindLabel        = "fws.cri.kind"
	sandboxLabel     = "fws.cri.sandbox"
	metadataLabel    = "fws.cri.metadata"
	labelsLabel      = "fws.cri.labels"
	annotationsLabel = "fws.cri.annotations"
	logLabel         = "fws.cri.log"
	imageLabel       = "fws.cri.image"
	configLabel      = "fws.cri.config"

	kindSandbox   = "sandbox"
	kindContainer = "container"
)

// Service is the runtime and image services over one engine.
type Service struct {
	runtimeapi.UnimplementedRuntimeServiceServer
	runtimeapi.UnimplementedImageServiceServer

	e            *engine.Engine
	sandboxImage string
	grpc         *grpc.Server

	// streams serves exec, attach and port-forward, from the first call
	// that needs it: a nested engine opens before its privileged
	// container's shim makes the directory the listener binds in.
	streamsMu     sync.Mutex
	streams       streaming.Server
	streamHTTP    *http.Server
	streamsClosed bool

	mu   sync.Mutex
	logs map[string]*logCopy

	// podCIDR is the node's pod CIDR, from the kubelet, which the pod
	// network takes as its subnet; netMu orders the network's changes.
	netMu   sync.Mutex
	podCIDR netip.Prefix

	// rootUsage remembers what each image root an earlier store unpacked
	// takes: the store names a root by its image's digest and never
	// changes one it has unpacked.
	rootsMu   sync.Mutex
	rootUsage map[string]image.Usage
}

// New serves CRI over e. Its sandboxes run sandboxImage.
func New(e *engine.Engine, sandboxImage string) *Service {
	s := &Service{e: e, sandboxImage: sandboxImage, grpc: grpc.NewServer(), logs: map[string]*logCopy{},
		rootUsage: map[string]image.Usage{}}
	runtimeapi.RegisterRuntimeServiceServer(s.grpc, s)
	runtimeapi.RegisterImageServiceServer(s.grpc, s)
	s.resumeLogs()
	return s
}

// Serves reports whether r is a CRI call: gRPC, over HTTP/2.
func Serves(r *http.Request) bool {
	return r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.grpc.ServeHTTP(w, r)
}

// Close stops serving streams and copying the containers' logs.
func (s *Service) Close() {
	s.streamsMu.Lock()
	s.streamsClosed = true
	if s.streamHTTP != nil {
		s.streamHTTP.Close()
	}
	s.streamsMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.logs {
		l.stop()
		delete(s.logs, id)
	}
}

// grpcError gives an engine error the gRPC code CRI's clients test for.
func grpcError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch engine.KindOf(err) {
	case engine.KindNotFound:
		return status.Error(codes.NotFound, err.Error())
	case engine.KindInvalid:
		return status.Error(codes.InvalidArgument, err.Error())
	case engine.KindConflict:
		return status.Error(codes.FailedPrecondition, err.Error())
	case engine.KindUnsupported:
		return status.Error(codes.Unimplemented, err.Error())
	case engine.KindUnauthorized, engine.KindForbidden:
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return status.Error(codes.Unknown, err.Error())
}

func isNotFound(err error) bool {
	if engine.KindOf(err) == engine.KindNotFound {
		return true
	}
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.NotFound
}

func encodeLabel(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeLabel[T any](labels map[string]string, key string) (T, error) {
	var v T
	raw, ok := labels[key]
	if !ok {
		return v, fmt.Errorf("label %s is missing", key)
	}
	return v, json.Unmarshal([]byte(raw), &v)
}

func stringMap(labels map[string]string, key string) map[string]string {
	m, err := decodeLabel[map[string]string](labels, key)
	if err != nil || m == nil {
		return map[string]string{}
	}
	return m
}

// matchesSelector reports whether labels carry every pair of selector.
func matchesSelector(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// nanos is t in nanoseconds since the epoch, 0 for the zero time.
func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}
	return t
}

// listKind lists the engine's containers that hold CRI objects of kind.
func (s *Service) listKind(kind string, extra ...string) []string {
	f := engine.Filters{"label": append([]string{kindLabel + "=" + kind}, extra...)}
	var ids []string
	for _, c := range s.e.List(engine.ListOptions{All: true, Filters: f}) {
		ids = append(ids, c.ID)
	}
	return ids
}

// resolve finds the engine's container of kind by its ID or a unique
// prefix of it, as the CRI's clients name them.
func (s *Service) resolve(kind, ref string) (string, error) {
	if ref == "" {
		return "", status.Error(codes.InvalidArgument, "an ID is required")
	}
	var found []string
	for _, id := range s.listKind(kind) {
		if strings.HasPrefix(id, ref) {
			found = append(found, id)
		}
	}
	switch len(found) {
	case 0:
		return "", status.Errorf(codes.NotFound, "%s %q not found", kind, ref)
	case 1:
		return found[0], nil
	}
	return "", status.Errorf(codes.InvalidArgument, "%s ID %q is ambiguous", kind, ref)
}

func (s *Service) Version(ctx context.Context, req *runtimeapi.VersionRequest) (*runtimeapi.VersionResponse, error) {
	return &runtimeapi.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       "fun-with-sandboxes",
		RuntimeVersion:    engine.Version,
		RuntimeApiVersion: "v1",
	}, nil
}

func (s *Service) Status(ctx context.Context, req *runtimeapi.StatusRequest) (*runtimeapi.StatusResponse, error) {
	res := &runtimeapi.StatusResponse{Status: &runtimeapi.RuntimeStatus{Conditions: []*runtimeapi.RuntimeCondition{
		{Type: runtimeapi.RuntimeReady, Status: true},
		{Type: runtimeapi.NetworkReady, Status: true},
	}}}
	if req.Verbose {
		res.Info = map[string]string{"sandboxImage": s.sandboxImage, "root": s.e.Root()}
	}
	return res, nil
}

// UpdateRuntimeConfig takes the node's pod CIDR, which the controller
// manager assigns the node: the pod network's subnet, from which each
// sandbox takes its address.
func (s *Service) UpdateRuntimeConfig(ctx context.Context, req *runtimeapi.UpdateRuntimeConfigRequest) (*runtimeapi.UpdateRuntimeConfigResponse, error) {
	cidr := req.GetRuntimeConfig().GetNetworkConfig().GetPodCidr()
	if cidr == "" {
		return &runtimeapi.UpdateRuntimeConfigResponse{}, nil
	}
	p, err := podSubnet(cidr)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.netMu.Lock()
	s.podCIDR = p
	s.netMu.Unlock()
	if err := s.ensurePodNetwork(); err != nil {
		return nil, grpcError(err)
	}
	return &runtimeapi.UpdateRuntimeConfigResponse{}, nil
}
