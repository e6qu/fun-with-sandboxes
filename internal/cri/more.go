package cri

import (
	"context"
	"time"

	"github.com/moby/moby/api/types/events"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// The streaming lists, behind the kubelet's CRIListStreaming gate, send
// what the plain lists return, in one message.

func (s *Service) StreamPodSandboxes(req *runtimeapi.StreamPodSandboxesRequest, srv grpc.ServerStreamingServer[runtimeapi.StreamPodSandboxesResponse]) error {
	r, err := s.ListPodSandbox(srv.Context(), &runtimeapi.ListPodSandboxRequest{Filter: req.Filter})
	if err != nil {
		return err
	}
	return srv.Send(&runtimeapi.StreamPodSandboxesResponse{PodSandboxes: r.Items})
}

func (s *Service) StreamContainers(req *runtimeapi.StreamContainersRequest, srv grpc.ServerStreamingServer[runtimeapi.StreamContainersResponse]) error {
	r, err := s.ListContainers(srv.Context(), &runtimeapi.ListContainersRequest{Filter: req.Filter})
	if err != nil {
		return err
	}
	return srv.Send(&runtimeapi.StreamContainersResponse{Containers: r.Containers})
}

func (s *Service) StreamContainerStats(req *runtimeapi.StreamContainerStatsRequest, srv grpc.ServerStreamingServer[runtimeapi.StreamContainerStatsResponse]) error {
	r, err := s.ListContainerStats(srv.Context(), &runtimeapi.ListContainerStatsRequest{Filter: req.Filter})
	if err != nil {
		return err
	}
	return srv.Send(&runtimeapi.StreamContainerStatsResponse{ContainerStats: r.Stats})
}

func (s *Service) StreamPodSandboxStats(req *runtimeapi.StreamPodSandboxStatsRequest, srv grpc.ServerStreamingServer[runtimeapi.StreamPodSandboxStatsResponse]) error {
	r, err := s.ListPodSandboxStats(srv.Context(), &runtimeapi.ListPodSandboxStatsRequest{Filter: req.Filter})
	if err != nil {
		return err
	}
	return srv.Send(&runtimeapi.StreamPodSandboxStatsResponse{PodSandboxStats: r.Stats})
}

func (s *Service) StreamImages(req *runtimeapi.StreamImagesRequest, srv grpc.ServerStreamingServer[runtimeapi.StreamImagesResponse]) error {
	r, err := s.ListImages(srv.Context(), &runtimeapi.ListImagesRequest{Filter: req.Filter})
	if err != nil {
		return err
	}
	return srv.Send(&runtimeapi.StreamImagesResponse{Images: r.Images})
}

func (s *Service) StreamPodSandboxMetrics(req *runtimeapi.StreamPodSandboxMetricsRequest, srv grpc.ServerStreamingServer[runtimeapi.StreamPodSandboxMetricsResponse]) error {
	r, err := s.ListPodSandboxMetrics(srv.Context(), &runtimeapi.ListPodSandboxMetricsRequest{})
	if err != nil {
		return err
	}
	return srv.Send(&runtimeapi.StreamPodSandboxMetricsResponse{PodSandboxMetrics: r.PodMetrics})
}

// containerEventType is the CRI event an engine action is, if any.
func containerEventType(a events.Action) (runtimeapi.ContainerEventType, bool) {
	switch a {
	case events.ActionCreate:
		return runtimeapi.ContainerEventType_CONTAINER_CREATED_EVENT, true
	case events.ActionStart:
		return runtimeapi.ContainerEventType_CONTAINER_STARTED_EVENT, true
	case events.ActionDie:
		return runtimeapi.ContainerEventType_CONTAINER_STOPPED_EVENT, true
	case events.ActionDestroy:
		return runtimeapi.ContainerEventType_CONTAINER_DELETED_EVENT, true
	}
	return 0, false
}

// GetContainerEvents streams the lifecycle of the pods' sandboxes and
// containers, which the kubelet's evented PLEG reads in place of polling:
// each event carries its pod's sandbox status and the status of each of
// its containers left, as containerd's carry them.
func (s *Service) GetContainerEvents(_ *runtimeapi.GetEventsRequest, srv grpc.ServerStreamingServer[runtimeapi.ContainerEventResponse]) error {
	_, ch, cancel := s.e.Subscribe(time.Time{})
	defer cancel()
	for {
		var m events.Message
		select {
		case <-srv.Context().Done():
			return nil
		case m = <-ch:
		}
		if m.Type != events.ContainerEventType {
			continue
		}
		t, ok := containerEventType(m.Action)
		if !ok {
			continue
		}
		var sandbox string
		switch m.Actor.Attributes[kindLabel] {
		case kindSandbox:
			sandbox = m.Actor.ID
		case kindContainer:
			sandbox = m.Actor.Attributes[sandboxLabel]
		default:
			continue
		}
		ev := &runtimeapi.ContainerEventResponse{ContainerId: m.Actor.ID, ContainerEventType: t, CreatedAt: m.TimeNano}
		if in, err := s.e.Inspect(sandbox); err == nil {
			if st, err := sandboxStatus(in); err == nil {
				ev.PodSandboxStatus = st
			}
		}
		for _, id := range s.sandboxContainers(sandbox) {
			in, err := s.e.Inspect(id)
			if err != nil {
				continue
			}
			if st, err := containerStatus(in); err == nil {
				ev.ContainersStatuses = append(ev.ContainersStatuses, st)
			}
		}
		if err := srv.Send(ev); err != nil {
			return err
		}
	}
}

// The metrics the runtime gives the kubelet in place of cAdvisor's, by
// cAdvisor's names: per container, labelled by its ID, name and image;
// per pod, the network its sandbox holds, labelled by interface too.
var (
	containerMetricLabels = []string{"id", "name", "image"}
	networkMetricLabels   = []string{"id", "name", "image", "interface"}
	metricDescriptors     = []*runtimeapi.MetricDescriptor{
		{Name: "container_cpu_usage_seconds_total", Help: "Cumulative CPU time consumed, in whole seconds.", LabelKeys: containerMetricLabels},
		{Name: "container_memory_usage_bytes", Help: "Current memory usage, in bytes.", LabelKeys: containerMetricLabels},
		{Name: "container_memory_working_set_bytes", Help: "Current working set, in bytes.", LabelKeys: containerMetricLabels},
		{Name: "container_processes", Help: "Number of processes running inside the container.", LabelKeys: containerMetricLabels},
		{Name: "container_network_receive_bytes_total", Help: "Cumulative count of bytes received.", LabelKeys: networkMetricLabels},
		{Name: "container_network_transmit_bytes_total", Help: "Cumulative count of bytes transmitted.", LabelKeys: networkMetricLabels},
		{Name: "container_network_receive_errors_total", Help: "Cumulative count of errors encountered while receiving.", LabelKeys: networkMetricLabels},
		{Name: "container_network_transmit_errors_total", Help: "Cumulative count of errors encountered while transmitting.", LabelKeys: networkMetricLabels},
	}
)

func (s *Service) ListMetricDescriptors(ctx context.Context, req *runtimeapi.ListMetricDescriptorsRequest) (*runtimeapi.ListMetricDescriptorsResponse, error) {
	return &runtimeapi.ListMetricDescriptorsResponse{Descriptors: metricDescriptors}, nil
}

func metric(name string, t runtimeapi.MetricType, at int64, v uint64, labels ...string) *runtimeapi.Metric {
	return &runtimeapi.Metric{Name: name, Timestamp: at, MetricType: t, LabelValues: labels, Value: &runtimeapi.UInt64Value{Value: v}}
}

// ListPodSandboxMetrics measures each pod: its containers' CPU, memory and
// processes, and its sandbox's network.
func (s *Service) ListPodSandboxMetrics(ctx context.Context, req *runtimeapi.ListPodSandboxMetricsRequest) (*runtimeapi.ListPodSandboxMetricsResponse, error) {
	out := []*runtimeapi.PodSandboxMetrics{}
	for _, id := range s.listKind(kindSandbox) {
		pause, err := s.sample(ctx, id)
		if err != nil {
			continue
		}
		pm := &runtimeapi.PodSandboxMetrics{PodSandboxId: id}
		at := pause.Read.UnixNano()
		for _, n := range sortedKeys(pause.Networks) {
			ns := pause.Networks[n]
			l := []string{id, "POD", s.sandboxImage, n}
			pm.Metrics = append(pm.Metrics,
				metric("container_network_receive_bytes_total", runtimeapi.MetricType_COUNTER, at, ns.RxBytes, l...),
				metric("container_network_transmit_bytes_total", runtimeapi.MetricType_COUNTER, at, ns.TxBytes, l...),
				metric("container_network_receive_errors_total", runtimeapi.MetricType_COUNTER, at, ns.RxErrors, l...),
				metric("container_network_transmit_errors_total", runtimeapi.MetricType_COUNTER, at, ns.TxErrors, l...))
		}
		for _, c := range s.sandboxContainers(id) {
			in, err := s.e.Inspect(c)
			if err != nil {
				continue
			}
			st, err := containerStatus(in)
			if err != nil {
				continue
			}
			r, err := s.sample(ctx, c)
			if err != nil {
				continue
			}
			at := r.Read.UnixNano()
			l := []string{c, st.GetMetadata().GetName(), st.GetImage().GetImage()}
			pm.ContainerMetrics = append(pm.ContainerMetrics, &runtimeapi.ContainerMetrics{ContainerId: c, Metrics: []*runtimeapi.Metric{
				metric("container_cpu_usage_seconds_total", runtimeapi.MetricType_COUNTER, at, r.CPUStats.CPUUsage.TotalUsage/uint64(time.Second), l...),
				metric("container_memory_usage_bytes", runtimeapi.MetricType_GAUGE, at, r.MemoryStats.Usage, l...),
				metric("container_memory_working_set_bytes", runtimeapi.MetricType_GAUGE, at, r.MemoryStats.Usage, l...),
				metric("container_processes", runtimeapi.MetricType_GAUGE, at, r.PidsStats.Current, l...),
			}})
		}
		out = append(out, pm)
	}
	return &runtimeapi.ListPodSandboxMetricsResponse{PodMetrics: out}, nil
}

// UpdatePodSandboxResources takes the pod's new totals, after the kubelet
// has resized its containers with UpdateContainerResources, which the
// engine enforces one container at a time. There is no pod-wide limit to
// change, as containerd has none of its own either: it hands the call to
// its plugins.
func (s *Service) UpdatePodSandboxResources(ctx context.Context, req *runtimeapi.UpdatePodSandboxResourcesRequest) (*runtimeapi.UpdatePodSandboxResourcesResponse, error) {
	if _, err := s.resolve(kindSandbox, req.PodSandboxId); err != nil {
		return nil, err
	}
	return &runtimeapi.UpdatePodSandboxResourcesResponse{}, nil
}

// errNoCheckpoint answers checkpoint and restore: CRIU is Linux's, and
// macOS gives an unprivileged process no way to save another's state.
var errNoCheckpoint = status.Error(codes.Unimplemented, "checkpoints are not supported: macOS cannot save a running process's state")

func (s *Service) CheckpointContainer(context.Context, *runtimeapi.CheckpointContainerRequest) (*runtimeapi.CheckpointContainerResponse, error) {
	return nil, errNoCheckpoint
}

func (s *Service) CheckpointPod(context.Context, *runtimeapi.CheckpointPodRequest) (*runtimeapi.CheckpointPodResponse, error) {
	return nil, errNoCheckpoint
}

func (s *Service) RestorePod(context.Context, *runtimeapi.RestorePodRequest) (*runtimeapi.RestorePodResponse, error) {
	return nil, errNoCheckpoint
}
