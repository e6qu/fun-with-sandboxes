package cri

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// sample is the engine's sample of container id now.
func (s *Service) sample(ctx context.Context, id string) (container.StatsResponse, error) {
	var out container.StatsResponse
	err := s.e.Stats(ctx, id, false, true, func(r container.StatsResponse) error {
		out = r
		return nil
	})
	return out, err
}

func cpuStats(r container.StatsResponse) *runtimeapi.CpuUsage {
	return &runtimeapi.CpuUsage{
		Timestamp:            r.Read.UnixNano(),
		UsageCoreNanoSeconds: &runtimeapi.UInt64Value{Value: r.CPUStats.CPUUsage.TotalUsage},
	}
}

func memoryStats(r container.StatsResponse) *runtimeapi.MemoryUsage {
	m := &runtimeapi.MemoryUsage{
		Timestamp:       r.Read.UnixNano(),
		WorkingSetBytes: &runtimeapi.UInt64Value{Value: r.MemoryStats.Usage},
		UsageBytes:      &runtimeapi.UInt64Value{Value: r.MemoryStats.Usage},
		RssBytes:        &runtimeapi.UInt64Value{Value: r.MemoryStats.Usage},
	}
	if l := r.MemoryStats.Limit; l > 0 && l >= r.MemoryStats.Usage {
		m.AvailableBytes = &runtimeapi.UInt64Value{Value: l - r.MemoryStats.Usage}
	}
	return m
}

// containerStats is the CRI stats of container id: its CPU time, its
// memory and what its writes since create take.
func (s *Service) containerStats(ctx context.Context, id string) (*runtimeapi.ContainerStats, container.StatsResponse, error) {
	in, err := s.e.Inspect(id)
	if err != nil {
		return nil, container.StatsResponse{}, grpcError(err)
	}
	st, err := containerStatus(in)
	if err != nil {
		return nil, container.StatsResponse{}, err
	}
	r, err := s.sample(ctx, id)
	if err != nil {
		return nil, r, grpcError(err)
	}
	out := &runtimeapi.ContainerStats{
		Attributes: &runtimeapi.ContainerAttributes{Id: id, Metadata: st.Metadata, Labels: st.Labels, Annotations: st.Annotations},
		Cpu:        cpuStats(r),
		Memory:     memoryStats(r),
	}
	for _, c := range s.e.List(engine.ListOptions{All: true, Size: true, Filters: engine.Filters{"id": {id}}}) {
		out.WritableLayer = &runtimeapi.FilesystemUsage{
			Timestamp: r.Read.UnixNano(),
			FsId:      &runtimeapi.FilesystemIdentifier{Mountpoint: filepath.Join(s.e.Root(), "stacks")},
			UsedBytes: &runtimeapi.UInt64Value{Value: uint64(max(c.SizeRw, 0))},
		}
	}
	return out, r, nil
}

func (s *Service) ContainerStats(ctx context.Context, req *runtimeapi.ContainerStatsRequest) (*runtimeapi.ContainerStatsResponse, error) {
	id, err := s.resolve(kindContainer, req.ContainerId)
	if err != nil {
		return nil, err
	}
	st, _, err := s.containerStats(ctx, id)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ContainerStatsResponse{Stats: st}, nil
}

func (s *Service) ListContainerStats(ctx context.Context, req *runtimeapi.ListContainerStatsRequest) (*runtimeapi.ListContainerStatsResponse, error) {
	f := req.GetFilter()
	list, err := s.ListContainers(ctx, &runtimeapi.ListContainersRequest{Filter: &runtimeapi.ContainerFilter{
		Id: f.GetId(), PodSandboxId: f.GetPodSandboxId(), LabelSelector: f.GetLabelSelector(),
	}})
	if err != nil {
		return nil, err
	}
	out := []*runtimeapi.ContainerStats{}
	for _, c := range list.Containers {
		st, _, err := s.containerStats(ctx, c.Id)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		out = append(out, st)
	}
	return &runtimeapi.ListContainerStatsResponse{Stats: out}, nil
}

// podSandboxStats sums the CPU, memory and processes of sandbox id's pause
// container and its containers, and gives the network its pause container
// holds.
func (s *Service) podSandboxStats(ctx context.Context, id string) (*runtimeapi.PodSandboxStats, error) {
	in, err := s.e.Inspect(id)
	if err != nil {
		return nil, grpcError(err)
	}
	st, err := sandboxStatus(in)
	if err != nil {
		return nil, err
	}
	pause, err := s.sample(ctx, id)
	if err != nil {
		return nil, grpcError(err)
	}
	now := time.Now().UnixNano()
	cpu, mem, procs := pause.CPUStats.CPUUsage.TotalUsage, pause.MemoryStats.Usage, pause.PidsStats.Current
	linux := &runtimeapi.LinuxPodSandboxStats{}
	for _, c := range s.sandboxContainers(id) {
		cs, r, err := s.containerStats(ctx, c)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		cpu += r.CPUStats.CPUUsage.TotalUsage
		mem += r.MemoryStats.Usage
		procs += r.PidsStats.Current
		linux.Containers = append(linux.Containers, cs)
	}
	linux.Cpu = &runtimeapi.CpuUsage{Timestamp: now, UsageCoreNanoSeconds: &runtimeapi.UInt64Value{Value: cpu}}
	linux.Memory = &runtimeapi.MemoryUsage{
		Timestamp:       now,
		WorkingSetBytes: &runtimeapi.UInt64Value{Value: mem},
		UsageBytes:      &runtimeapi.UInt64Value{Value: mem},
		RssBytes:        &runtimeapi.UInt64Value{Value: mem},
	}
	linux.Process = &runtimeapi.ProcessUsage{Timestamp: now, ProcessCount: &runtimeapi.UInt64Value{Value: procs}}
	names := make([]string, 0, len(pause.Networks))
	for n := range pause.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 0 {
		network := &runtimeapi.NetworkUsage{Timestamp: now}
		for i, n := range names {
			ns := pause.Networks[n]
			iface := &runtimeapi.NetworkInterfaceUsage{
				Name:     n,
				RxBytes:  &runtimeapi.UInt64Value{Value: ns.RxBytes},
				RxErrors: &runtimeapi.UInt64Value{Value: ns.RxErrors},
				TxBytes:  &runtimeapi.UInt64Value{Value: ns.TxBytes},
				TxErrors: &runtimeapi.UInt64Value{Value: ns.TxErrors},
			}
			if i == 0 {
				network.DefaultInterface = iface
			} else {
				network.Interfaces = append(network.Interfaces, iface)
			}
		}
		linux.Network = network
	}
	return &runtimeapi.PodSandboxStats{
		Attributes: &runtimeapi.PodSandboxAttributes{Id: id, Metadata: st.Metadata, Labels: st.Labels, Annotations: st.Annotations},
		Linux:      linux,
	}, nil
}

func (s *Service) PodSandboxStats(ctx context.Context, req *runtimeapi.PodSandboxStatsRequest) (*runtimeapi.PodSandboxStatsResponse, error) {
	id, err := s.resolve(kindSandbox, req.PodSandboxId)
	if err != nil {
		return nil, err
	}
	st, err := s.podSandboxStats(ctx, id)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.PodSandboxStatsResponse{Stats: st}, nil
}

func (s *Service) ListPodSandboxStats(ctx context.Context, req *runtimeapi.ListPodSandboxStatsRequest) (*runtimeapi.ListPodSandboxStatsResponse, error) {
	f := req.GetFilter()
	out := []*runtimeapi.PodSandboxStats{}
	for _, id := range s.listKind(kindSandbox) {
		if f.GetId() != "" && !strings.HasPrefix(id, f.GetId()) {
			continue
		}
		st, err := s.podSandboxStats(ctx, id)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		if !matchesSelector(st.Attributes.Labels, f.GetLabelSelector()) {
			continue
		}
		out = append(out, st)
	}
	return &runtimeapi.ListPodSandboxStatsResponse{Stats: out}, nil
}
