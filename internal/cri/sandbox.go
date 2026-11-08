package cri

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// sandboxConfig is what a sandbox's containers take from its config.
type sandboxConfig struct {
	HostNetwork bool `json:"hostNetwork,omitempty"`
}

// createRequest is the engine's create request for a container config and
// host config in the Docker API's JSON shape.
func createRequest(config, host map[string]any) (container.CreateRequest, error) {
	b, err := json.Marshal(map[string]any{"HostConfig": host})
	if err != nil {
		return container.CreateRequest{}, err
	}
	var req container.CreateRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return req, err
	}
	b, err = json.Marshal(config)
	if err != nil {
		return req, err
	}
	req.Config = &container.Config{}
	if err := json.Unmarshal(b, req.Config); err != nil {
		return req, err
	}
	return req, nil
}

func (s *Service) RunPodSandbox(ctx context.Context, req *runtimeapi.RunPodSandboxRequest) (*runtimeapi.RunPodSandboxResponse, error) {
	cfg := req.GetConfig()
	m := cfg.GetMetadata()
	if m == nil || m.Name == "" || m.Namespace == "" || m.Uid == "" {
		return nil, status.Error(codes.InvalidArgument, "a sandbox's metadata needs its name, namespace and UID")
	}
	if req.RuntimeHandler != "" {
		return nil, status.Errorf(codes.InvalidArgument, "the runtime has no handler %q", req.RuntimeHandler)
	}
	img, err := s.ensureImage(ctx, s.sandboxImage, nil)
	if err != nil {
		return nil, err
	}
	sc := sandboxConfig{HostNetwork: cfg.GetLinux().GetSecurityContext().GetNamespaceOptions().GetNetwork() == runtimeapi.NamespaceMode_NODE}
	labels := map[string]string{
		kindLabel:        kindSandbox,
		metadataLabel:    encodeLabel(m),
		labelsLabel:      encodeLabel(cfg.Labels),
		annotationsLabel: encodeLabel(cfg.Annotations),
		logLabel:         cfg.LogDirectory,
		configLabel:      encodeLabel(sc),
	}
	config := map[string]any{"Image": img, "Labels": labels}
	host := map[string]any{}
	if sc.HostNetwork {
		host["NetworkMode"] = "host"
	} else if cfg.Hostname != "" {
		config["Hostname"] = cfg.Hostname
	}
	if d := cfg.DnsConfig; d != nil {
		host["Dns"], host["DnsSearch"], host["DnsOptions"] = d.Servers, d.Searches, d.Options
	}
	exposed := map[string]struct{}{}
	bindings := map[string][]map[string]string{}
	for _, pm := range cfg.PortMappings {
		port := fmt.Sprintf("%d/%s", pm.ContainerPort, strings.ToLower(pm.Protocol.String()))
		exposed[port] = struct{}{}
		if pm.HostPort > 0 {
			bindings[port] = append(bindings[port], map[string]string{"HostIp": pm.HostIp, "HostPort": strconv.Itoa(int(pm.HostPort))})
		}
	}
	config["ExposedPorts"], host["PortBindings"] = exposed, bindings
	create, err := createRequest(config, host)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	name := fmt.Sprintf("k8s_POD_%s_%s_%s_%d", m.Name, m.Namespace, m.Uid, m.Attempt)
	res, err := s.e.Create(engine.CreateOptions{Name: name}, create)
	if err != nil {
		return nil, grpcError(err)
	}
	if err := s.e.Start(res.ID); err != nil {
		s.e.Remove(res.ID, engine.RemoveOptions{Force: true})
		return nil, grpcError(err)
	}
	return &runtimeapi.RunPodSandboxResponse{PodSandboxId: res.ID}, nil
}

// sandboxContainers are the IDs of the containers created in sandbox id.
func (s *Service) sandboxContainers(id string) []string {
	return s.listKind(kindContainer, sandboxLabel+"="+id)
}

func (s *Service) StopPodSandbox(ctx context.Context, req *runtimeapi.StopPodSandboxRequest) (*runtimeapi.StopPodSandboxResponse, error) {
	id, err := s.resolve(kindSandbox, req.PodSandboxId)
	if isNotFound(err) {
		return &runtimeapi.StopPodSandboxResponse{}, nil
	} else if err != nil {
		return nil, err
	}
	zero := 0
	for _, c := range append(s.sandboxContainers(id), id) {
		if err := s.e.Stop(c, &zero, ""); err != nil && !isNotFound(err) && engine.KindOf(err) != engine.KindNotModified {
			return nil, grpcError(err)
		}
	}
	return &runtimeapi.StopPodSandboxResponse{}, nil
}

func (s *Service) RemovePodSandbox(ctx context.Context, req *runtimeapi.RemovePodSandboxRequest) (*runtimeapi.RemovePodSandboxResponse, error) {
	id, err := s.resolve(kindSandbox, req.PodSandboxId)
	if isNotFound(err) {
		return &runtimeapi.RemovePodSandboxResponse{}, nil
	} else if err != nil {
		return nil, err
	}
	for _, c := range append(s.sandboxContainers(id), id) {
		s.stopLog(c)
		if err := s.e.Remove(c, engine.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !isNotFound(err) {
			return nil, grpcError(err)
		}
	}
	return &runtimeapi.RemovePodSandboxResponse{}, nil
}

// sandboxStatus is the CRI status of the sandbox whose pause container in
// describes it.
func sandboxStatus(in container.InspectResponse) (*runtimeapi.PodSandboxStatus, error) {
	labels := in.Config.Labels
	meta, err := decodeLabel[runtimeapi.PodSandboxMetadata](labels, metadataLabel)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sandbox %s: %v", in.ID, err)
	}
	sc, _ := decodeLabel[sandboxConfig](labels, configLabel)
	state := runtimeapi.PodSandboxState_SANDBOX_NOTREADY
	if in.State != nil && in.State.Running {
		state = runtimeapi.PodSandboxState_SANDBOX_READY
	}
	network := &runtimeapi.PodSandboxNetworkStatus{}
	mode := runtimeapi.NamespaceMode_POD
	if sc.HostNetwork {
		mode = runtimeapi.NamespaceMode_NODE
	} else if in.NetworkSettings != nil {
		for _, ep := range in.NetworkSettings.Networks {
			if ep == nil {
				continue
			}
			for _, a := range []string{addr(ep.IPAddress), addr(ep.GlobalIPv6Address)} {
				switch {
				case a == "":
				case network.Ip == "":
					network.Ip = a
				default:
					network.AdditionalIps = append(network.AdditionalIps, &runtimeapi.PodIP{Ip: a})
				}
			}
			if network.Ip != "" {
				break
			}
		}
	}
	return &runtimeapi.PodSandboxStatus{
		Id:        in.ID,
		Metadata:  &meta,
		State:     state,
		CreatedAt: nanos(parseTime(in.Created)),
		Network:   network,
		Linux: &runtimeapi.LinuxPodSandboxStatus{Namespaces: &runtimeapi.Namespace{Options: &runtimeapi.NamespaceOption{
			Network: mode, Pid: runtimeapi.NamespaceMode_CONTAINER, Ipc: runtimeapi.NamespaceMode_POD,
		}}},
		Labels:      stringMap(labels, labelsLabel),
		Annotations: stringMap(labels, annotationsLabel),
	}, nil
}

func addr(a interface {
	IsValid() bool
	String() string
}) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

func (s *Service) PodSandboxStatus(ctx context.Context, req *runtimeapi.PodSandboxStatusRequest) (*runtimeapi.PodSandboxStatusResponse, error) {
	id, err := s.resolve(kindSandbox, req.PodSandboxId)
	if err != nil {
		return nil, err
	}
	in, err := s.e.Inspect(id)
	if err != nil {
		return nil, grpcError(err)
	}
	st, err := sandboxStatus(in)
	if err != nil {
		return nil, err
	}
	res := &runtimeapi.PodSandboxStatusResponse{Status: st, Timestamp: time.Now().UnixNano()}
	if req.Verbose {
		b, _ := json.Marshal(in)
		res.Info = map[string]string{"info": string(b)}
	}
	return res, nil
}

func (s *Service) ListPodSandbox(ctx context.Context, req *runtimeapi.ListPodSandboxRequest) (*runtimeapi.ListPodSandboxResponse, error) {
	f := req.GetFilter()
	out := []*runtimeapi.PodSandbox{}
	for _, id := range s.listKind(kindSandbox) {
		if f.GetId() != "" && !strings.HasPrefix(id, f.GetId()) {
			continue
		}
		in, err := s.e.Inspect(id)
		if err != nil {
			continue
		}
		st, err := sandboxStatus(in)
		if err != nil {
			continue
		}
		if f.GetState() != nil && f.GetState().State != st.State {
			continue
		}
		if !matchesSelector(st.Labels, f.GetLabelSelector()) {
			continue
		}
		out = append(out, &runtimeapi.PodSandbox{
			Id: st.Id, Metadata: st.Metadata, State: st.State, CreatedAt: st.CreatedAt,
			Labels: st.Labels, Annotations: st.Annotations,
		})
	}
	return &runtimeapi.ListPodSandboxResponse{Items: out}, nil
}
