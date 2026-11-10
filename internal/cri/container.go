package cri

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/moby/moby/api/types/container"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// containerRecord is what CreateContainer was asked for that the engine's
// container does not hold itself.
type containerRecord struct {
	Mounts []*runtimeapi.Mount `json:"mounts,omitempty"`
}

func (s *Service) CreateContainer(ctx context.Context, req *runtimeapi.CreateContainerRequest) (*runtimeapi.CreateContainerResponse, error) {
	cfg := req.GetConfig()
	m := cfg.GetMetadata()
	if m == nil || m.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "a container's metadata needs its name")
	}
	if cfg.GetImage().GetImage() == "" {
		return nil, status.Error(codes.InvalidArgument, "a container needs an image")
	}
	sandbox, err := s.resolve(kindSandbox, req.PodSandboxId)
	if err != nil {
		return nil, err
	}
	sin, err := s.e.Inspect(sandbox)
	if err != nil {
		return nil, grpcError(err)
	}
	if sin.State == nil || !sin.State.Running {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is not ready", sandbox)
	}
	smeta, err := decodeLabel[*runtimeapi.PodSandboxMetadata](sin.Config.Labels, metadataLabel)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sandbox %s: %v", sandbox, err)
	}
	sc, _ := decodeLabel[sandboxConfig](sin.Config.Labels, configLabel)
	img, err := s.e.ImageInspect(cfg.Image.Image)
	if err != nil {
		return nil, grpcError(err)
	}
	logPath := ""
	if cfg.LogPath != "" {
		logPath = filepath.Join(sin.Config.Labels[logLabel], cfg.LogPath)
	}
	labels := map[string]string{
		kindLabel:        kindContainer,
		sandboxLabel:     sandbox,
		metadataLabel:    encodeLabel(m),
		labelsLabel:      encodeLabel(cfg.Labels),
		annotationsLabel: encodeLabel(cfg.Annotations),
		logLabel:         logPath,
		imageLabel:       cfg.Image.Image,
		configLabel:      encodeLabel(containerRecord{Mounts: cfg.Mounts}),
	}
	// The pod's containers see its hostname, as containerd gives them.
	hostname := sin.Config.Hostname
	if sc.HostNetwork {
		hostname, _ = os.Hostname()
	}
	env := []string{"HOSTNAME=" + hostname}
	for _, kv := range cfg.Envs {
		env = append(env, kv.Key+"="+string(kv.Value))
	}
	config := map[string]any{
		"Image": img.ID, "Labels": labels, "Env": env, "WorkingDir": cfg.WorkingDir,
		"Tty": cfg.Tty, "OpenStdin": cfg.Stdin, "StdinOnce": cfg.StdinOnce, "AttachStdin": cfg.Stdin,
	}
	// CRI's command replaces the image's entrypoint, and its args the
	// image's command, as Docker's Entrypoint and Cmd do.
	if len(cfg.Command) > 0 {
		config["Entrypoint"] = cfg.Command
	}
	if len(cfg.Args) > 0 {
		config["Cmd"] = cfg.Args
	}
	network := "container:" + sandbox
	if sc.HostNetwork {
		network = "host"
	}
	host := map[string]any{"NetworkMode": network}
	var mounts []map[string]any
	for _, mt := range cfg.Mounts {
		mounts = append(mounts, map[string]any{
			"Type": "bind", "Source": mt.HostPath, "Target": mt.ContainerPath, "ReadOnly": mt.Readonly,
		})
	}
	host["Mounts"] = mounts
	create, err := createRequest(config, host)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	name := fmt.Sprintf("k8s_%s_%s_%s_%s_%d", m.Name, smeta.Name, smeta.Namespace, smeta.Uid, m.Attempt)
	res, err := s.e.Create(engine.CreateOptions{Name: name}, create)
	if err != nil {
		return nil, grpcError(err)
	}
	return &runtimeapi.CreateContainerResponse{ContainerId: res.ID}, nil
}

func (s *Service) StartContainer(ctx context.Context, req *runtimeapi.StartContainerRequest) (*runtimeapi.StartContainerResponse, error) {
	id, err := s.resolve(kindContainer, req.ContainerId)
	if err != nil {
		return nil, err
	}
	in, err := s.e.Inspect(id)
	if err != nil {
		return nil, grpcError(err)
	}
	if in.State != nil && in.State.Status != container.StateCreated {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s is %s, not created", id, in.State.Status)
	}
	if err := s.e.Start(id); err != nil {
		return nil, grpcError(err)
	}
	// The engine keeps all the container's output, so the copy, begun
	// once the container runs, starts from its first line.
	if err := s.startLog(id, in.Config.Labels[logLabel], time.Time{}); err != nil {
		return nil, status.Errorf(codes.Internal, "container %s's log: %v", id, err)
	}
	return &runtimeapi.StartContainerResponse{}, nil
}

func (s *Service) StopContainer(ctx context.Context, req *runtimeapi.StopContainerRequest) (*runtimeapi.StopContainerResponse, error) {
	id, err := s.resolve(kindContainer, req.ContainerId)
	if err != nil {
		return nil, err
	}
	timeout := int(req.Timeout)
	if err := s.e.Stop(id, &timeout, ""); err != nil && engine.KindOf(err) != engine.KindNotModified {
		return nil, grpcError(err)
	}
	return &runtimeapi.StopContainerResponse{}, nil
}

func (s *Service) RemoveContainer(ctx context.Context, req *runtimeapi.RemoveContainerRequest) (*runtimeapi.RemoveContainerResponse, error) {
	id, err := s.resolve(kindContainer, req.ContainerId)
	if isNotFound(err) {
		return &runtimeapi.RemoveContainerResponse{}, nil
	} else if err != nil {
		return nil, err
	}
	s.stopLog(id)
	if err := s.e.Remove(id, engine.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !isNotFound(err) {
		return nil, grpcError(err)
	}
	return &runtimeapi.RemoveContainerResponse{}, nil
}

func containerState(st *container.State) runtimeapi.ContainerState {
	if st == nil {
		return runtimeapi.ContainerState_CONTAINER_UNKNOWN
	}
	switch st.Status {
	case container.StateCreated:
		return runtimeapi.ContainerState_CONTAINER_CREATED
	case container.StateRunning, container.StatePaused, container.StateRestarting:
		return runtimeapi.ContainerState_CONTAINER_RUNNING
	case container.StateExited, container.StateDead:
		return runtimeapi.ContainerState_CONTAINER_EXITED
	}
	return runtimeapi.ContainerState_CONTAINER_UNKNOWN
}

// containerStatus is the CRI status of the container in describes.
func containerStatus(in container.InspectResponse) (*runtimeapi.ContainerStatus, error) {
	labels := in.Config.Labels
	meta, err := decodeLabel[*runtimeapi.ContainerMetadata](labels, metadataLabel)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "container %s: %v", in.ID, err)
	}
	rec, _ := decodeLabel[containerRecord](labels, configLabel)
	st := &runtimeapi.ContainerStatus{
		Id:          in.ID,
		Metadata:    meta,
		State:       containerState(in.State),
		CreatedAt:   nanos(parseTime(in.Created)),
		Image:       &runtimeapi.ImageSpec{Image: labels[imageLabel]},
		ImageRef:    in.Image,
		ImageId:     in.Image,
		Labels:      stringMap(labels, labelsLabel),
		Annotations: stringMap(labels, annotationsLabel),
		Mounts:      rec.Mounts,
		LogPath:     labels[logLabel],
	}
	if in.State != nil {
		st.StartedAt = nanos(parseTime(in.State.StartedAt))
		if st.State == runtimeapi.ContainerState_CONTAINER_EXITED {
			st.FinishedAt = nanos(parseTime(in.State.FinishedAt))
			st.ExitCode = int32(in.State.ExitCode)
			switch {
			case in.State.OOMKilled:
				st.Reason = "OOMKilled"
			case in.State.ExitCode == 0:
				st.Reason = "Completed"
			default:
				st.Reason = "Error"
			}
			st.Message = in.State.Error
		}
	}
	return st, nil
}

func (s *Service) ContainerStatus(ctx context.Context, req *runtimeapi.ContainerStatusRequest) (*runtimeapi.ContainerStatusResponse, error) {
	id, err := s.resolve(kindContainer, req.ContainerId)
	if err != nil {
		return nil, err
	}
	in, err := s.e.Inspect(id)
	if err != nil {
		return nil, grpcError(err)
	}
	st, err := containerStatus(in)
	if err != nil {
		return nil, err
	}
	res := &runtimeapi.ContainerStatusResponse{Status: st}
	if req.Verbose {
		b, _ := json.Marshal(in)
		res.Info = map[string]string{"info": string(b)}
	}
	return res, nil
}

func (s *Service) ListContainers(ctx context.Context, req *runtimeapi.ListContainersRequest) (*runtimeapi.ListContainersResponse, error) {
	f := req.GetFilter()
	var extra []string
	if f.GetPodSandboxId() != "" {
		sandbox, err := s.resolve(kindSandbox, f.GetPodSandboxId())
		if isNotFound(err) {
			return &runtimeapi.ListContainersResponse{Containers: []*runtimeapi.Container{}}, nil
		} else if err != nil {
			return nil, err
		}
		extra = append(extra, sandboxLabel+"="+sandbox)
	}
	out := []*runtimeapi.Container{}
	for _, id := range s.listKind(kindContainer, extra...) {
		if f.GetId() != "" && !strings.HasPrefix(id, f.GetId()) {
			continue
		}
		in, err := s.e.Inspect(id)
		if err != nil {
			continue
		}
		st, err := containerStatus(in)
		if err != nil {
			continue
		}
		if f.GetState() != nil && f.GetState().State != st.State {
			continue
		}
		if !matchesSelector(st.Labels, f.GetLabelSelector()) {
			continue
		}
		out = append(out, &runtimeapi.Container{
			Id: st.Id, PodSandboxId: in.Config.Labels[sandboxLabel], Metadata: st.Metadata,
			Image: st.Image, ImageRef: st.ImageRef, ImageId: st.ImageId, State: st.State, CreatedAt: st.CreatedAt,
			Labels: st.Labels, Annotations: st.Annotations,
		})
	}
	return &runtimeapi.ListContainersResponse{Containers: out}, nil
}

// UpdateContainerResources applies the CPU and memory limits a Linux
// resources request names, which the engine enforces as it does docker
// update's.
func (s *Service) UpdateContainerResources(ctx context.Context, req *runtimeapi.UpdateContainerResourcesRequest) (*runtimeapi.UpdateContainerResourcesResponse, error) {
	id, err := s.resolve(kindContainer, req.ContainerId)
	if err != nil {
		return nil, err
	}
	r := req.GetLinux()
	if r == nil {
		return &runtimeapi.UpdateContainerResourcesResponse{}, nil
	}
	var u container.UpdateConfig
	u.Memory = r.MemoryLimitInBytes
	if r.CpuQuota > 0 && r.CpuPeriod > 0 {
		u.NanoCPUs = r.CpuQuota * 1e9 / r.CpuPeriod
	}
	u.CPUShares = r.CpuShares
	if _, err := s.e.Update(id, u); err != nil {
		return nil, grpcError(err)
	}
	return &runtimeapi.UpdateContainerResourcesResponse{}, nil
}

func (s *Service) ExecSync(ctx context.Context, req *runtimeapi.ExecSyncRequest) (*runtimeapi.ExecSyncResponse, error) {
	id, err := s.resolve(kindContainer, req.ContainerId)
	if err != nil {
		return nil, err
	}
	if len(req.Cmd) == 0 {
		return nil, status.Error(codes.InvalidArgument, "exec needs a command")
	}
	exec, err := s.e.ExecCreate(id, container.ExecCreateRequest{Cmd: req.Cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return nil, grpcError(err)
	}
	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- s.e.ExecStart(exec, false, nil, engine.ExecIO{Stdout: &stdout, Stderr: &stderr}) }()
	var timeout <-chan time.Time
	if req.Timeout > 0 {
		t := time.NewTimer(time.Duration(req.Timeout) * time.Second)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case err = <-done:
	case <-timeout:
		s.killExec(exec)
		<-done
		return nil, status.Errorf(codes.DeadlineExceeded, "command %q timed out after %ds", strings.Join(req.Cmd, " "), req.Timeout)
	case <-ctx.Done():
		s.killExec(exec)
		<-done
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if err != nil {
		return nil, grpcError(err)
	}
	x, err := s.e.ExecInspect(exec)
	if err != nil {
		return nil, grpcError(err)
	}
	code := int32(-1)
	if x.ExitCode != nil {
		code = int32(*x.ExitCode)
	}
	return &runtimeapi.ExecSyncResponse{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: code}, nil
}

// killExec ends a running exec's process, which runs as one of the
// host's.
func (s *Service) killExec(id string) {
	if x, err := s.e.ExecInspect(id); err == nil && x.Running && x.Pid > 0 {
		syscall.Kill(x.Pid, syscall.SIGKILL)
	}
}
