package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/volume"
	"go.yaml.in/yaml/v3"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// podman kube play creates pods from Kubernetes YAML (POST /play/kube),
// podman kube down removes them (DELETE /play/kube), and podman kube
// generate writes a pod or container as a Pod (GET /generate/kube). Of
// Kubernetes' kinds, Pods and Deployments make pods, a Deployment's of
// one replica, as Podman runs it; ConfigMaps give containers variables;
// PersistentVolumeClaims are named volumes. What a pod's spec asks for
// that the engine cannot honour is refused, as any option is.

type kubeObject struct {
	Kind     string    `yaml:"kind"`
	Metadata kubeMeta  `yaml:"metadata"`
	Spec     yaml.Node `yaml:"spec"`
	Data     map[string]string
}

type kubeMeta struct {
	Name        string            `yaml:"name,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

type kubePodSpec struct {
	Containers            []kubeContainer `yaml:"containers"`
	InitContainers        []kubeContainer `yaml:"initContainers,omitempty"`
	Volumes               []kubeVolume    `yaml:"volumes,omitempty"`
	RestartPolicy         string          `yaml:"restartPolicy,omitempty"`
	Hostname              string          `yaml:"hostname,omitempty"`
	HostNetwork           bool            `yaml:"hostNetwork,omitempty"`
	ShareProcessNamespace bool            `yaml:"shareProcessNamespace,omitempty"`
}

type kubeDeploymentSpec struct {
	Replicas *int `yaml:"replicas"`
	Template struct {
		Metadata kubeMeta    `yaml:"metadata"`
		Spec     kubePodSpec `yaml:"spec"`
	} `yaml:"template"`
}

type kubeContainer struct {
	Name            string               `yaml:"name"`
	Image           string               `yaml:"image"`
	Command         []string             `yaml:"command,omitempty"`
	Args            []string             `yaml:"args,omitempty"`
	WorkingDir      string               `yaml:"workingDir,omitempty"`
	Env             []kubeEnv            `yaml:"env,omitempty"`
	Ports           []kubePort           `yaml:"ports,omitempty"`
	VolumeMounts    []kubeMount          `yaml:"volumeMounts,omitempty"`
	Resources       *kubeResources       `yaml:"resources,omitempty"`
	SecurityContext *kubeSecurityContext `yaml:"securityContext,omitempty"`
	Stdin           bool                 `yaml:"stdin,omitempty"`
	TTY             bool                 `yaml:"tty,omitempty"`
}

type kubeEnv struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value,omitempty"`
	ValueFrom *struct {
		ConfigMapKeyRef *struct {
			Name     string `yaml:"name"`
			Key      string `yaml:"key"`
			Optional bool   `yaml:"optional"`
		} `yaml:"configMapKeyRef"`
	} `yaml:"valueFrom,omitempty"`
}

type kubePort struct {
	ContainerPort int    `yaml:"containerPort"`
	HostPort      int    `yaml:"hostPort,omitempty"`
	HostIP        string `yaml:"hostIP,omitempty"`
	Protocol      string `yaml:"protocol,omitempty"`
}

type kubeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	ReadOnly  bool   `yaml:"readOnly,omitempty"`
	SubPath   string `yaml:"subPath,omitempty"`
}

type kubeVolume struct {
	Name     string `yaml:"name"`
	HostPath *struct {
		Path string `yaml:"path"`
	} `yaml:"hostPath,omitempty"`
	EmptyDir              *struct{} `yaml:"emptyDir,omitempty"`
	PersistentVolumeClaim *struct {
		ClaimName string `yaml:"claimName"`
		ReadOnly  bool   `yaml:"readOnly"`
	} `yaml:"persistentVolumeClaim,omitempty"`
	Other map[string]any `yaml:",inline"`
}

type kubeResources struct {
	Limits   map[string]string `yaml:"limits,omitempty"`
	Requests map[string]string `yaml:"requests,omitempty"`
}

type kubeSecurityContext struct {
	RunAsUser              *int64 `yaml:"runAsUser,omitempty"`
	RunAsGroup             *int64 `yaml:"runAsGroup,omitempty"`
	Privileged             *bool  `yaml:"privileged,omitempty"`
	ReadOnlyRootFilesystem *bool  `yaml:"readOnlyRootFilesystem,omitempty"`
}

// kubePod is a pod to play: its name, labels and spec.
type kubePod struct {
	Name   string
	Labels map[string]string
	Spec   kubePodSpec
}

// parseKube reads the YAML documents of r: the pods its Pods and
// Deployments make, its ConfigMaps by name and its claims' names.
func parseKube(r io.Reader) (pods []kubePod, configMaps map[string]map[string]string, claims []string, err error) {
	configMaps = map[string]map[string]string{}
	dec := yaml.NewDecoder(r)
	for {
		var o kubeObject
		if err := dec.Decode(&o); err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, nil, engineErr(engine.KindInvalid, "kube YAML: %v", err)
		}
		switch o.Kind {
		case "":
			continue
		case "Pod":
			var spec kubePodSpec
			if err := o.Spec.Decode(&spec); err != nil {
				return nil, nil, nil, engineErr(engine.KindInvalid, "pod %s: %v", o.Metadata.Name, err)
			}
			pods = append(pods, kubePod{Name: o.Metadata.Name, Labels: o.Metadata.Labels, Spec: spec})
		case "Deployment":
			var spec kubeDeploymentSpec
			if err := o.Spec.Decode(&spec); err != nil {
				return nil, nil, nil, engineErr(engine.KindInvalid, "deployment %s: %v", o.Metadata.Name, err)
			}
			if spec.Replicas != nil && *spec.Replicas > 1 {
				return nil, nil, nil, engineErr(engine.KindUnsupported, "deployment %s asks for %d replicas: kube play runs one, as Podman does", o.Metadata.Name, *spec.Replicas)
			}
			labels := spec.Template.Metadata.Labels
			if labels == nil {
				labels = o.Metadata.Labels
			}
			pods = append(pods, kubePod{Name: o.Metadata.Name + "-pod", Labels: labels, Spec: spec.Template.Spec})
		case "ConfigMap":
			configMaps[o.Metadata.Name] = o.Data
		case "PersistentVolumeClaim":
			claims = append(claims, o.Metadata.Name)
		default:
			return nil, nil, nil, engineErr(engine.KindUnsupported, "kind %s is not supported by kube play: Pods, Deployments, ConfigMaps and PersistentVolumeClaims are", o.Kind)
		}
		if o.Kind != "ConfigMap" && o.Metadata.Name == "" {
			return nil, nil, nil, engineErr(engine.KindInvalid, "a %s needs a name", o.Kind)
		}
	}
	return pods, configMaps, claims, nil
}

func (s *Server) libpodKubeRoutes() {
	m := s.lp
	m.HandleFunc("POST /play/kube", s.kubePlay)
	m.HandleFunc("POST /kube/play", s.kubePlay)
	m.HandleFunc("DELETE /play/kube", s.kubeDown)
	m.HandleFunc("DELETE /kube/play", s.kubeDown)
	m.HandleFunc("GET /generate/kube", s.kubeGenerate)
	m.HandleFunc("GET /kube/generate", s.kubeGenerate)
}

// kubePlay creates, and unless start is false starts, the pods the YAML
// describes, with the volumes its claims name.
func (s *Server) kubePlay(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pods, configMaps, claims, err := parseKube(r.Body)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	start := q.Get("start") == "" || boolParam(q.Get("start"))
	report := map[string]any{"Pods": []any{}, "Volumes": []any{}, "Secrets": []any{}, "RmReport": []any{}, "StopReport": []any{}, "ServiceContainerID": ""}
	if boolParam(q.Get("replace")) {
		for _, kp := range pods {
			if p, err := s.pods.get(kp.Name); err == nil {
				if _, err := s.removePod(p); err != nil {
					writeEngineError(w, err)
					return
				}
			}
		}
	}
	for _, c := range claims {
		if _, err := s.e.VolumeInspect(c); err != nil {
			if _, err := s.e.VolumeCreate(volume.CreateRequest{Name: c}); err != nil {
				writeEngineError(w, err)
				return
			}
		}
		report["Volumes"] = append(report["Volumes"].([]any), map[string]string{"Name": c})
	}
	for _, kp := range pods {
		p, containers, inits, err := s.playPod(r, kp, configMaps, q["network"], start)
		if err != nil {
			if p.ID != "" {
				s.removePod(p)
			}
			writeEngineError(w, err)
			return
		}
		report["Pods"] = append(report["Pods"].([]any), map[string]any{
			"ID": p.ID, "Containers": containers, "InitContainers": inits, "Logs": []string{}, "ContainerErrors": []string{},
		})
	}
	writeJSON(w, http.StatusOK, report)
}

// playPod creates pod kp, its containers after its init containers, and
// starts it: each init container runs to its end, and must succeed,
// before the next and before the containers start.
func (s *Server) playPod(r *http.Request, kp kubePod, configMaps map[string]map[string]string, networks []string, start bool) (pod, []string, []string, error) {
	if kp.Spec.HostNetwork {
		return pod{}, nil, nil, engineErr(engine.KindUnsupported, "pod %s asks for the host's network, which no container here may have", kp.Name)
	}
	vols := map[string]kubeVolume{}
	for _, v := range kp.Spec.Volumes {
		if v.HostPath == nil && v.EmptyDir == nil && v.PersistentVolumeClaim == nil {
			kinds := make([]string, 0, len(v.Other))
			for k := range v.Other {
				kinds = append(kinds, k)
			}
			sort.Strings(kinds)
			return pod{}, nil, nil, engineErr(engine.KindUnsupported, "pod %s's volume %s (%s) is not supported: hostPath, emptyDir and persistentVolumeClaim volumes are", kp.Name, v.Name, strings.Join(kinds, ", "))
		}
		vols[v.Name] = v
	}
	restart := map[string]string{"": "always", "Always": "always", "OnFailure": "on-failure", "Never": "no"}[kp.Spec.RestartPolicy]
	if restart == "" {
		return pod{}, nil, nil, engineErr(engine.KindInvalid, "pod %s's restart policy %q is not Always, OnFailure or Never", kp.Name, kp.Spec.RestartPolicy)
	}
	// The pod's published ports are its infra container's, which holds its
	// network.
	infra := map[string]any{}
	var ports []map[string]any
	for _, c := range append(append([]kubeContainer{}, kp.Spec.InitContainers...), kp.Spec.Containers...) {
		for _, pt := range c.Ports {
			if pt.HostPort == 0 {
				continue
			}
			proto := strings.ToLower(pt.Protocol)
			if proto == "" {
				proto = "tcp"
			}
			ports = append(ports, map[string]any{"container_port": pt.ContainerPort, "host_port": pt.HostPort, "host_ip": pt.HostIP, "protocol": proto, "range": 1})
		}
	}
	infra["portmappings"] = ports
	if len(networks) > 0 {
		nets := map[string]any{}
		for _, n := range networks {
			nets[n] = map[string]any{}
		}
		infra["Networks"] = nets
	}
	var infraSpec specGen
	if err := remarshal(infra, &infraSpec); err != nil {
		return pod{}, nil, nil, err
	}
	share := []string{"net", "ipc", "uts"}
	if kp.Spec.ShareProcessNamespace {
		share = append(share, "pid")
	}
	hostname := kp.Spec.Hostname
	if hostname == "" {
		hostname = kp.Name
	}
	p, err := s.newPod(podSpec{Name: kp.Name, Labels: kp.Labels, Share: share, Hostname: hostname}, infraSpec)
	if err != nil {
		return pod{}, nil, nil, err
	}
	create := func(c kubeContainer, policy string) (string, error) {
		spec, err := kubeSpec(kp, p, c, policy, vols, configMaps)
		if err != nil {
			return "", err
		}
		// kube play pulls an image the engine lacks.
		if _, err := s.e.ImageInspect(c.Image); err != nil {
			if _, err := s.e.Images().Pull(r.Context(), c.Image, nil, func(string, string) {}); err != nil {
				return "", err
			}
		}
		res, err := s.createContainer(spec)
		return res.ID, err
	}
	var inits, containers []string
	for _, c := range kp.Spec.InitContainers {
		id, err := create(c, "no")
		if err != nil {
			return p, nil, nil, err
		}
		inits = append(inits, id)
	}
	for _, c := range kp.Spec.Containers {
		id, err := create(c, restart)
		if err != nil {
			return p, nil, nil, err
		}
		containers = append(containers, id)
	}
	if !start {
		return p, containers, inits, nil
	}
	if err := s.e.Start(p.InfraID); err != nil {
		return p, nil, nil, err
	}
	for i, id := range inits {
		wait, err := s.e.Wait(id, container.WaitConditionNextExit)
		if err != nil {
			return p, nil, nil, err
		}
		if err := s.e.Start(id); err != nil {
			return p, nil, nil, err
		}
		select {
		case res := <-wait:
			if res.StatusCode != 0 {
				return p, nil, nil, engineErr(engine.KindInvalid, "pod %s's init container %s exited with %d", kp.Name, kp.Spec.InitContainers[i].Name, res.StatusCode)
			}
		case <-r.Context().Done():
			return p, nil, nil, r.Context().Err()
		}
	}
	for _, id := range containers {
		if err := s.e.Start(id); err != nil {
			return p, nil, nil, err
		}
	}
	return p, containers, inits, nil
}

// kubeSpec is container c of pod p as Podman's specification of a
// container.
func kubeSpec(kp kubePod, p pod, c kubeContainer, restart string, vols map[string]kubeVolume, configMaps map[string]map[string]string) (specGen, error) {
	if c.Name == "" || c.Image == "" {
		return specGen{}, engineErr(engine.KindInvalid, "pod %s has a container without a name or an image", kp.Name)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		switch {
		case e.ValueFrom == nil:
			env[e.Name] = e.Value
		case e.ValueFrom.ConfigMapKeyRef != nil:
			ref := e.ValueFrom.ConfigMapKeyRef
			v, ok := configMaps[ref.Name][ref.Key]
			if !ok && !ref.Optional {
				return specGen{}, engineErr(engine.KindInvalid, "container %s's variable %s names key %s of ConfigMap %s, which the YAML does not hold", c.Name, e.Name, ref.Key, ref.Name)
			}
			if ok {
				env[e.Name] = v
			}
		default:
			return specGen{}, engineErr(engine.KindUnsupported, "container %s's variable %s comes from a source kube play does not read: values and ConfigMap keys are", c.Name, e.Name)
		}
	}
	spec := map[string]any{
		"name": kp.Name + "-" + c.Name, "pod": p.ID, "image": c.Image, "entrypoint": c.Command, "command": c.Args,
		"env": env, "work_dir": c.WorkingDir, "terminal": c.TTY, "stdin": c.Stdin, "restart_policy": restart,
		"labels": map[string]string{kubeContainerLabel: c.Name},
	}
	var mounts, volumes []map[string]any
	for _, m := range c.VolumeMounts {
		v, ok := vols[m.Name]
		if !ok {
			return specGen{}, engineErr(engine.KindInvalid, "container %s mounts volume %s, which pod %s does not have", c.Name, m.Name, kp.Name)
		}
		opts := []string{}
		if m.ReadOnly {
			opts = append(opts, "ro")
		}
		switch {
		case v.HostPath != nil:
			if m.SubPath != "" {
				return specGen{}, engineErr(engine.KindUnsupported, "container %s mounts a subpath of hostPath volume %s: mount its directory", c.Name, m.Name)
			}
			mounts = append(mounts, map[string]any{"type": "bind", "source": v.HostPath.Path, "destination": m.MountPath, "options": opts})
		case v.EmptyDir != nil:
			mounts = append(mounts, map[string]any{"type": "volume", "source": "", "destination": m.MountPath, "options": opts})
		case v.PersistentVolumeClaim != nil:
			if v.PersistentVolumeClaim.ReadOnly {
				opts = append(opts, "ro")
			}
			volumes = append(volumes, map[string]any{"Name": v.PersistentVolumeClaim.ClaimName, "Dest": m.MountPath, "Options": opts, "SubPath": m.SubPath})
		}
	}
	spec["mounts"], spec["volumes"] = mounts, volumes
	if c.Resources != nil {
		limits := map[string]any{}
		if v, ok := c.Resources.Limits["memory"]; ok {
			n, err := parseQuantity(v)
			if err != nil {
				return specGen{}, engineErr(engine.KindInvalid, "container %s's memory limit %q: %v", c.Name, v, err)
			}
			limits["memory"] = map[string]any{"limit": int64(n)}
		}
		if v, ok := c.Resources.Limits["cpu"]; ok {
			n, err := parseQuantity(v)
			if err != nil {
				return specGen{}, engineErr(engine.KindInvalid, "container %s's CPU limit %q: %v", c.Name, v, err)
			}
			limits["cpu"] = map[string]any{"quota": int64(math.Round(n * 100000)), "period": 100000}
		}
		if len(limits) > 0 {
			spec["resource_limits"] = limits
		}
	}
	if sc := c.SecurityContext; sc != nil {
		if sc.RunAsUser != nil {
			user := strconv.FormatInt(*sc.RunAsUser, 10)
			if sc.RunAsGroup != nil {
				user += ":" + strconv.FormatInt(*sc.RunAsGroup, 10)
			}
			spec["user"] = user
		}
		if sc.Privileged != nil {
			spec["privileged"] = *sc.Privileged
		}
		if sc.ReadOnlyRootFilesystem != nil {
			spec["read_only_filesystem"] = *sc.ReadOnlyRootFilesystem
		}
	}
	var out specGen
	return out, remarshal(spec, &out)
}

// kubeContainerLabel names a container's name within its pod, which
// kube generate writes.
const kubeContainerLabel = "io.fws.kube.container"

// parseQuantity is a Kubernetes quantity's value: decimal or binary
// suffixes, or a CPU count in thousandths.
func parseQuantity(v string) (float64, error) {
	units := []struct {
		suffix string
		scale  float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"m", 1e-3}, {"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}}
	scale := 1.0
	for _, u := range units {
		if n, ok := strings.CutSuffix(v, u.suffix); ok {
			v, scale = n, u.scale
			break
		}
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return 0, errors.New("not a quantity")
	}
	return n * scale, nil
}

// remarshal decodes v, Podman's JSON shape, into out.
func remarshal(v any, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// kubeDown removes the pods the YAML describes.
func (s *Server) kubeDown(w http.ResponseWriter, r *http.Request) {
	pods, _, _, err := parseKube(r.Body)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	stops, rms := []any{}, []any{}
	for _, kp := range pods {
		p, err := s.pods.get(kp.Name)
		if err != nil {
			continue
		}
		removed, err := s.removePod(p)
		if err != nil {
			writeEngineError(w, err)
			return
		}
		stops = append(stops, map[string]any{"Id": p.ID, "Errs": []string{}, "RawInput": kp.Name})
		rms = append(rms, map[string]any{"Id": p.ID, "Err": nil, "RemovedCtrs": removed})
	}
	writeJSON(w, http.StatusOK, map[string]any{"Pods": []any{}, "Volumes": []any{}, "StopReport": stops, "RmReport": rms, "Secrets": []any{}, "ServiceContainerID": ""})
}

// kubeGenerate writes each pod or container names names as a Pod: its
// containers' images, commands, variables beyond their images', working
// directories, published ports and limits.
func (s *Server) kubeGenerate(w http.ResponseWriter, r *http.Request) {
	names := r.URL.Query()["names"]
	if len(names) == 0 {
		writeError(w, http.StatusBadRequest, "no pod or container named")
		return
	}
	var out bytes.Buffer
	out.WriteString("# Save the output of this file and use kubectl create -f to import it into Kubernetes.\n#\n# Created with fwsd " + engine.Version + "\n")
	for i, name := range names {
		doc, err := s.kubePodDoc(name)
		if err != nil {
			writeEngineError(w, err)
			return
		}
		if i > 0 {
			out.WriteString("---\n")
		}
		b, err := yaml.Marshal(doc)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out.Write(b)
	}
	w.Header().Set("Content-Type", "text/vnd.yaml")
	w.WriteHeader(http.StatusOK)
	w.Write(out.Bytes())
}

// kubeDoc is a Pod as kube generate writes it.
type kubeDoc struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   kubeMeta    `yaml:"metadata"`
	Spec       kubePodSpec `yaml:"spec"`
}

// kubePodDoc is the pod, or the container, named name as a Pod.
func (s *Server) kubePodDoc(name string) (kubeDoc, error) {
	var ids []string
	podName, prefix := "", ""
	var ports map[string][]map[string]string
	if p, err := s.pods.get(name); err == nil {
		podName, prefix = p.Name, p.Name+"-"
		for _, m := range s.podMembers(p) {
			if m.ID == p.InfraID {
				in, err := s.e.Inspect(m.ID)
				if err != nil {
					return kubeDoc{}, err
				}
				ports = bindingsOf(in.HostConfig)
				continue
			}
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
	} else {
		in, err := s.e.Inspect(name)
		if err != nil {
			return kubeDoc{}, err
		}
		ids = []string{in.ID}
		podName = strings.TrimPrefix(in.Name, "/") + "-pod"
		ports = bindingsOf(in.HostConfig)
	}
	doc := kubeDoc{APIVersion: "v1", Kind: "Pod", Metadata: kubeMeta{Name: podName, Labels: map[string]string{"app": podName}}}
	for _, id := range ids {
		in, err := s.e.Inspect(id)
		if err != nil {
			return kubeDoc{}, err
		}
		c := kubeContainer{Image: in.Config.Image, WorkingDir: in.Config.WorkingDir}
		c.Name = in.Config.Labels[kubeContainerLabel]
		if c.Name == "" {
			c.Name = strings.TrimPrefix(strings.TrimPrefix(in.Name, "/"), prefix)
		}
		img, _ := s.e.ImageInspect(in.Image)
		var imageEnv []string
		if img.Config != nil {
			imageEnv = img.Config.Env
			if c.WorkingDir == img.Config.WorkingDir {
				c.WorkingDir = ""
			}
			if !equalStrings(in.Config.Entrypoint, img.Config.Entrypoint) {
				c.Command = in.Config.Entrypoint
			}
			if !equalStrings(in.Config.Cmd, img.Config.Cmd) || c.Command != nil {
				c.Args = in.Config.Cmd
			}
		}
		for _, kv := range in.Config.Env {
			k, v, _ := strings.Cut(kv, "=")
			if k == "HOSTNAME" || containsString(imageEnv, kv) {
				continue
			}
			c.Env = append(c.Env, kubeEnv{Name: k, Value: v})
		}
		if hc := in.HostConfig; hc != nil {
			limits := map[string]string{}
			if hc.Memory > 0 {
				limits["memory"] = strconv.FormatInt(hc.Memory, 10)
			}
			if hc.NanoCPUs > 0 {
				limits["cpu"] = fmt.Sprintf("%dm", hc.NanoCPUs/1e6)
			} else if hc.CPUQuota > 0 && hc.CPUPeriod > 0 {
				limits["cpu"] = fmt.Sprintf("%dm", hc.CPUQuota*1000/hc.CPUPeriod)
			}
			if len(limits) > 0 {
				c.Resources = &kubeResources{Limits: limits}
			}
		}
		doc.Spec.Containers = append(doc.Spec.Containers, c)
	}
	// The pod's published ports go with its first container, as Podman
	// writes them.
	if len(doc.Spec.Containers) > 0 {
		keys := make([]string, 0, len(ports))
		for k := range ports {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			num, proto, _ := strings.Cut(k, "/")
			cp, _ := strconv.Atoi(num)
			for _, b := range ports[k] {
				hp, err := strconv.Atoi(b["HostPort"])
				if err != nil {
					continue
				}
				doc.Spec.Containers[0].Ports = append(doc.Spec.Containers[0].Ports, kubePort{ContainerPort: cp, HostPort: hp, Protocol: strings.ToUpper(proto)})
			}
		}
	}
	return doc, nil
}

// bindingsOf are a container's published ports, as PortBindings in a map.
func bindingsOf(hc *container.HostConfig) map[string][]map[string]string {
	out := map[string][]map[string]string{}
	if hc == nil {
		return out
	}
	for p, bs := range hc.PortBindings {
		for _, b := range bs {
			out[p.String()] = append(out[p.String()], map[string]string{"HostIp": b.HostIP.String(), "HostPort": b.HostPort})
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
