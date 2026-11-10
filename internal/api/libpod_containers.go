package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// specGen is the part of Podman's SpecGenerator, the body of POST
// /libpod/containers/create, that the engine honours. A field Podman sends
// that has no counterpart here fails the create in Docker's terms, as the
// engine would refuse it there.
type specGen struct {
	Name        string            `json:"name"`
	Pod         string            `json:"pod"`
	Image       string            `json:"image"`
	Entrypoint  []string          `json:"entrypoint"`
	Command     []string          `json:"command"`
	Env         map[string]string `json:"env"`
	Terminal    *bool             `json:"terminal"`
	Stdin       *bool             `json:"stdin"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StopSignal  *int              `json:"stop_signal"`
	StopTimeout *uint             `json:"stop_timeout"`
	LogConfig   *struct {
		Driver  string            `json:"driver"`
		Options map[string]string `json:"options"`
	} `json:"log_configuration"`
	RestartPolicy  string            `json:"restart_policy"`
	RestartRetries *uint             `json:"restart_tries"`
	PidNS          namespace         `json:"pidns"`
	UtsNS          namespace         `json:"utsns"`
	IpcNS          namespace         `json:"ipcns"`
	NetNS          namespace         `json:"netns"`
	UserNS         namespace         `json:"userns"`
	CgroupNS       namespace         `json:"cgroupns"`
	Hostname       string            `json:"hostname"`
	Sysctl         map[string]string `json:"sysctl"`
	Remove         *bool             `json:"remove"`
	VolumesFrom    []string          `json:"volumes_from"`
	Init           *bool             `json:"init"`
	Mounts         []struct {
		Destination string   `json:"destination"`
		Type        string   `json:"type"`
		Source      string   `json:"source"`
		Options     []string `json:"options"`
	} `json:"mounts"`
	Volumes []struct {
		Name        string
		Dest        string
		Options     []string
		IsAnonymous bool
		SubPath     string
	} `json:"volumes"`
	ImageVolumes []struct {
		Source      string
		Destination string
		ReadWrite   bool
		SubPath     string `json:"subPath"`
	} `json:"image_volumes"`
	Devices            []json.RawMessage `json:"devices"`
	ShmSize            *int64            `json:"shm_size"`
	WorkDir            string            `json:"work_dir"`
	Privileged         *bool             `json:"privileged"`
	User               string            `json:"user"`
	Groups             []string          `json:"groups"`
	CapAdd             []string          `json:"cap_add"`
	CapDrop            []string          `json:"cap_drop"`
	NoNewPrivileges    *bool             `json:"no_new_privileges"`
	ReadOnlyFilesystem *bool             `json:"read_only_filesystem"`
	PortMappings       []portMapping     `json:"portmappings"`
	PublishImagePorts  *bool             `json:"publish_image_ports"`
	Expose             map[string]string `json:"expose"`
	Networks           map[string]struct {
		StaticIPs []string `json:"static_ips"`
		Aliases   []string `json:"aliases"`
		StaticMAC string   `json:"static_mac"`
	} `json:"Networks"`
	DNSServers     []string `json:"dns_server"`
	DNSSearch      []string `json:"dns_search"`
	DNSOptions     []string `json:"dns_option"`
	HostAdd        []string `json:"hostadd"`
	ResourceLimits *struct {
		Memory *struct {
			Limit *int64 `json:"limit"`
			Swap  *int64 `json:"swap"`
		} `json:"memory"`
		CPU *struct {
			Shares *uint64 `json:"shares"`
			Quota  *int64  `json:"quota"`
			Period *uint64 `json:"period"`
		} `json:"cpu"`
		Pids *struct {
			Limit *int64 `json:"limit"`
		} `json:"pids"`
	} `json:"resource_limits"`
	Rlimits []struct {
		Type string `json:"type"`
		Hard uint64 `json:"hard"`
		Soft uint64 `json:"soft"`
	} `json:"r_limits"`
	OOMScoreAdj  *int `json:"oom_score_adj"`
	HealthConfig *struct {
		Test          []string `json:"Test"`
		StartPeriod   int64    `json:"StartPeriod"`
		StartInterval int64    `json:"StartInterval"`
		Interval      int64    `json:"Interval"`
		Timeout       int64    `json:"Timeout"`
		Retries       int      `json:"Retries"`
	} `json:"healthconfig"`
}

type namespace struct {
	NSMode string `json:"nsmode"`
	Value  string `json:"value"`
}

type portMapping struct {
	HostIP        string `json:"host_ip"`
	ContainerPort uint16 `json:"container_port"`
	HostPort      uint16 `json:"host_port"`
	Range         uint16 `json:"range"`
	Protocol      string `json:"protocol"`
}

// linuxSignals name the signals Podman's clients send by their Linux
// numbers, which differ from macOS's for some.
var linuxSignals = map[int]string{
	1: "SIGHUP", 2: "SIGINT", 3: "SIGQUIT", 4: "SIGILL", 5: "SIGTRAP", 6: "SIGABRT", 7: "SIGBUS", 8: "SIGFPE",
	9: "SIGKILL", 10: "SIGUSR1", 11: "SIGSEGV", 12: "SIGUSR2", 13: "SIGPIPE", 14: "SIGALRM", 15: "SIGTERM",
	17: "SIGCHLD", 18: "SIGCONT", 19: "SIGSTOP", 20: "SIGTSTP", 21: "SIGTTIN", 22: "SIGTTOU", 23: "SIGURG",
	24: "SIGXCPU", 25: "SIGXFSZ", 26: "SIGVTALRM", 27: "SIGPROF", 28: "SIGWINCH", 29: "SIGIO", 31: "SIGSYS",
}

// rlimitNames are the r_limits types by Docker's ulimit names.
var rlimitNames = map[string]string{
	"RLIMIT_AS": "as", "RLIMIT_CORE": "core", "RLIMIT_CPU": "cpu", "RLIMIT_DATA": "data", "RLIMIT_FSIZE": "fsize",
	"RLIMIT_LOCKS": "locks", "RLIMIT_MEMLOCK": "memlock", "RLIMIT_MSGQUEUE": "msgqueue", "RLIMIT_NICE": "nice",
	"RLIMIT_NOFILE": "nofile", "RLIMIT_NPROC": "nproc", "RLIMIT_RSS": "rss", "RLIMIT_RTPRIO": "rtprio",
	"RLIMIT_RTTIME": "rttime", "RLIMIT_SIGPENDING": "sigpending", "RLIMIT_STACK": "stack",
}

// nsMode is Docker's mode for a Podman namespace: "" for Podman's
// default, and an error for a mode Docker has no counterpart for.
func nsMode(kind string, ns namespace) (string, error) {
	switch ns.NSMode {
	case "", "default", "private":
		return "", nil
	case "host", "none", "shareable":
		return ns.NSMode, nil
	case "container":
		return "container:" + ns.Value, nil
	case "bridge":
		if kind == "network" {
			return "bridge", nil
		}
	}
	return "", engineErr(engine.KindUnsupported, "%s namespace mode %q is not supported", kind, ns.NSMode)
}

// engineErr is an error of the engine's kinds from the API.
func engineErr(kind engine.Kind, format string, a ...any) error {
	return &engine.Error{Kind: kind, Msg: fmt.Sprintf(format, a...)}
}

// dockerCreate is the Docker create request that does what spec asks.
func dockerCreate(spec specGen) (container.CreateRequest, error) {
	cfg := map[string]any{"Image": spec.Image, "Labels": spec.Labels}
	host := map[string]any{"Annotations": spec.Annotations}
	if spec.Entrypoint != nil {
		cfg["Entrypoint"] = spec.Entrypoint
	}
	if spec.Command != nil {
		cfg["Cmd"] = spec.Command
	}
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	cfg["Env"] = env
	if spec.Terminal != nil {
		cfg["Tty"] = *spec.Terminal
	}
	if spec.Stdin != nil && *spec.Stdin {
		cfg["OpenStdin"], cfg["AttachStdin"], cfg["StdinOnce"] = true, true, true
	}
	cfg["AttachStdout"], cfg["AttachStderr"] = true, true
	if spec.StopSignal != nil {
		name, ok := linuxSignals[*spec.StopSignal]
		if !ok {
			return container.CreateRequest{}, engineErr(engine.KindInvalid, "invalid stop signal %d", *spec.StopSignal)
		}
		cfg["StopSignal"] = name
	}
	if spec.StopTimeout != nil {
		cfg["StopTimeout"] = *spec.StopTimeout
	}
	cfg["Hostname"], cfg["User"], cfg["WorkingDir"] = spec.Hostname, spec.User, spec.WorkDir
	if h := spec.HealthConfig; h != nil {
		cfg["Healthcheck"] = map[string]any{"Test": h.Test, "StartPeriod": h.StartPeriod, "StartInterval": h.StartInterval,
			"Interval": h.Interval, "Timeout": h.Timeout, "Retries": h.Retries}
	}
	exposed := map[string]any{}
	for p, proto := range spec.Expose {
		for _, pr := range strings.Split(proto, ",") {
			if pr == "" {
				pr = "tcp"
			}
			exposed[p+"/"+pr] = struct{}{}
		}
	}
	bindings := map[string][]map[string]string{}
	for _, pm := range spec.PortMappings {
		protos := strings.Split(pm.Protocol, ",")
		n := pm.Range
		if n == 0 {
			n = 1
		}
		for _, pr := range protos {
			if pr == "" {
				pr = "tcp"
			}
			for i := uint16(0); i < n; i++ {
				port := fmt.Sprintf("%d/%s", pm.ContainerPort+i, pr)
				exposed[port] = struct{}{}
				hp := ""
				if pm.HostPort != 0 {
					hp = strconv.Itoa(int(pm.HostPort + i))
				}
				bindings[port] = append(bindings[port], map[string]string{"HostIp": pm.HostIP, "HostPort": hp})
			}
		}
	}
	cfg["ExposedPorts"] = exposed
	host["PortBindings"] = bindings
	if spec.PublishImagePorts != nil {
		host["PublishAllPorts"] = *spec.PublishImagePorts
	}

	var mounts []map[string]any
	var tmpfs = map[string]string{}
	for _, m := range spec.Mounts {
		ro := false
		for _, o := range m.Options {
			ro = ro || o == "ro"
		}
		switch m.Type {
		case "bind":
			mounts = append(mounts, map[string]any{"Type": "bind", "Source": m.Source, "Target": m.Destination, "ReadOnly": ro})
		case "tmpfs":
			tmpfs[m.Destination] = strings.Join(m.Options, ",")
		case "volume":
			mounts = append(mounts, map[string]any{"Type": "volume", "Source": m.Source, "Target": m.Destination, "ReadOnly": ro})
		default:
			return container.CreateRequest{}, engineErr(engine.KindUnsupported, "mount type %q is not supported", m.Type)
		}
	}
	for _, v := range spec.Volumes {
		ro := false
		for _, o := range v.Options {
			ro = ro || o == "ro"
		}
		m := map[string]any{"Type": "volume", "Source": v.Name, "Target": v.Dest, "ReadOnly": ro}
		if v.SubPath != "" {
			m["VolumeOptions"] = map[string]any{"Subpath": v.SubPath}
		}
		mounts = append(mounts, m)
	}
	for _, v := range spec.ImageVolumes {
		m := map[string]any{"Type": "image", "Source": v.Source, "Target": v.Destination, "ReadOnly": !v.ReadWrite}
		if v.SubPath != "" {
			m["ImageOptions"] = map[string]any{"Subpath": v.SubPath}
		}
		mounts = append(mounts, m)
	}
	host["Mounts"], host["Tmpfs"], host["VolumesFrom"] = mounts, tmpfs, spec.VolumesFrom
	if len(spec.Devices) > 0 {
		return container.CreateRequest{}, engineErr(engine.KindUnsupported, "devices are not supported: this engine runs without root")
	}

	var err error
	if host["PidMode"], err = nsMode("PID", spec.PidNS); err != nil {
		return container.CreateRequest{}, err
	}
	if host["IpcMode"], err = nsMode("IPC", spec.IpcNS); err != nil {
		return container.CreateRequest{}, err
	}
	if host["UTSMode"], err = nsMode("UTS", spec.UtsNS); err != nil {
		return container.CreateRequest{}, err
	}
	if host["UsernsMode"], err = nsMode("user", spec.UserNS); err != nil {
		return container.CreateRequest{}, err
	}
	if host["CgroupnsMode"], err = nsMode("cgroup", spec.CgroupNS); err != nil {
		return container.CreateRequest{}, err
	}
	netMode, err := nsMode("network", spec.NetNS)
	if err != nil {
		return container.CreateRequest{}, err
	}
	endpoints := map[string]any{}
	names := make([]string, 0, len(spec.Networks))
	for n := range spec.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		o := spec.Networks[n]
		es := map[string]any{"Aliases": o.Aliases, "MacAddress": o.StaticMAC}
		var ipam map[string]string
		for _, ip := range o.StaticIPs {
			if ipam == nil {
				ipam = map[string]string{}
			}
			if strings.Contains(ip, ":") {
				ipam["IPv6Address"] = ip
			} else {
				ipam["IPv4Address"] = ip
			}
		}
		if ipam != nil {
			es["IPAMConfig"] = ipam
		}
		endpoints[n] = es
	}
	if (netMode == "" || netMode == "bridge") && len(names) > 0 {
		netMode = names[0]
	}
	host["NetworkMode"] = netMode

	if spec.Privileged != nil {
		host["Privileged"] = *spec.Privileged
	}
	if spec.ReadOnlyFilesystem != nil {
		host["ReadonlyRootfs"] = *spec.ReadOnlyFilesystem
	}
	if spec.Init != nil {
		host["Init"] = *spec.Init
	}
	if spec.Remove != nil {
		host["AutoRemove"] = *spec.Remove
	}
	if spec.ShmSize != nil {
		host["ShmSize"] = *spec.ShmSize
	}
	if spec.OOMScoreAdj != nil {
		host["OomScoreAdj"] = *spec.OOMScoreAdj
	}
	host["CapAdd"], host["CapDrop"], host["GroupAdd"], host["ExtraHosts"] = spec.CapAdd, spec.CapDrop, spec.Groups, spec.HostAdd
	host["Dns"], host["DnsSearch"], host["DnsOptions"], host["Sysctls"] = spec.DNSServers, spec.DNSSearch, spec.DNSOptions, spec.Sysctl
	if spec.NoNewPrivileges != nil && *spec.NoNewPrivileges {
		host["SecurityOpt"] = []string{"no-new-privileges"}
	}
	if l := spec.LogConfig; l != nil && l.Driver != "" {
		driver := l.Driver
		// Podman's file driver is its name for Docker's json-file.
		if driver == "k8s-file" {
			driver = "json-file"
		}
		host["LogConfig"] = map[string]any{"Type": driver, "Config": l.Options}
	}
	if spec.RestartPolicy != "" {
		rp := map[string]any{"Name": spec.RestartPolicy}
		if spec.RestartRetries != nil {
			rp["MaximumRetryCount"] = *spec.RestartRetries
		}
		host["RestartPolicy"] = rp
	}
	if r := spec.ResourceLimits; r != nil {
		if r.Memory != nil {
			if r.Memory.Limit != nil {
				host["Memory"] = *r.Memory.Limit
			}
			// Podman's client asks for twice the memory as swap when it is
			// given none, Docker's default, which the engine applies.
			if r.Memory.Swap != nil && (r.Memory.Limit == nil || *r.Memory.Swap != 2**r.Memory.Limit) {
				host["MemorySwap"] = *r.Memory.Swap
			}
		}
		if r.CPU != nil {
			if r.CPU.Shares != nil {
				host["CpuShares"] = *r.CPU.Shares
			}
			if r.CPU.Quota != nil {
				host["CpuQuota"] = *r.CPU.Quota
			}
			if r.CPU.Period != nil {
				host["CpuPeriod"] = *r.CPU.Period
			}
		}
		if r.Pids != nil && r.Pids.Limit != nil {
			host["PidsLimit"] = *r.Pids.Limit
		}
	}
	var ulimits []map[string]any
	for _, l := range spec.Rlimits {
		name, ok := rlimitNames[strings.ToUpper(l.Type)]
		if !ok {
			name = strings.TrimPrefix(strings.ToLower(l.Type), "rlimit_")
		}
		ulimits = append(ulimits, map[string]any{"Name": name, "Soft": l.Soft, "Hard": l.Hard})
	}
	host["Ulimits"] = ulimits
	cfg["HostConfig"] = host
	cfg["NetworkingConfig"] = map[string]any{"EndpointsConfig": endpoints}

	b, err := json.Marshal(cfg)
	if err != nil {
		return container.CreateRequest{}, err
	}
	var req container.CreateRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return container.CreateRequest{}, engineErr(engine.KindInvalid, "%v", err)
	}
	return req, nil
}

func (s *Server) libpodContainerRoutes() {
	m := s.lp
	m.HandleFunc("POST /containers/create", s.libpodCreate)
	m.HandleFunc("GET /containers/json", s.libpodList)
	m.HandleFunc("GET /containers/{id}/json", s.libpodInspect)
	m.HandleFunc("GET /containers/{id}/exists", func(w http.ResponseWriter, r *http.Request) {
		_, err := s.e.Container(r.PathValue("id"))
		respondStatus(w, http.StatusNoContent, err)
	})
	m.HandleFunc("DELETE /containers/{id}", s.libpodRemove)
	m.HandleFunc("GET /containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) { s.logs(w, r, true) })
	m.HandleFunc("POST /containers/{id}/wait", s.libpodWait)
	m.HandleFunc("POST /exec/{id}/remove", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Force bool }
		if !decode(w, r, &body) {
			return
		}
		respondStatus(w, http.StatusOK, s.e.ExecRemove(r.PathValue("id"), body.Force))
	})
	m.HandleFunc("POST /containers/{id}/stop", s.simple(func(r *http.Request) error {
		q := r.URL.Query()
		var t *int
		if v := q.Get("timeout"); v != "" {
			n, _ := strconv.Atoi(v)
			t = &n
		} else {
			t = timeoutParam(r)
		}
		err := s.e.Stop(r.PathValue("id"), t, "")
		if engine.KindOf(err) == engine.KindNotModified && boolParam(q.Get("ignore")) {
			return nil
		}
		return err
	}, http.StatusNoContent))
}

func (s *Server) libpodCreate(w http.ResponseWriter, r *http.Request) {
	var spec specGen
	if !decode(w, r, &spec) {
		return
	}
	if spec.Pod != "" {
		p, err := s.pods.get(spec.Pod)
		if err != nil {
			writeEngineError(w, err)
			return
		}
		if spec.Labels == nil {
			spec.Labels = map[string]string{}
		}
		spec.Labels[podLabel] = p.ID
	}
	req, err := dockerCreate(spec)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	res, err := s.e.Create(engine.CreateOptions{Name: spec.Name}, req)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if res.Warnings == nil {
		res.Warnings = []string{}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"Id": res.ID, "Warnings": res.Warnings})
}

// libpodPortMappings are a container's published ports in Podman's shape.
func libpodPortMappings(ports map[string][]map[string]string) []portMapping {
	out := []portMapping{}
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
			out = append(out, portMapping{HostIP: b["HostIp"], ContainerPort: uint16(cp), HostPort: uint16(hp), Range: 1, Protocol: proto})
		}
	}
	return out
}

// asMap is v as a JSON object, for the fields Podman's shapes take from
// Docker's.
func asMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func (s *Server) libpodList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := engine.ParseFilters(q.Get("filters"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	podFilter := f["pod"]
	delete(f, "pod")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if last, _ := strconv.Atoi(q.Get("last")); last > 0 {
		limit = last
	}
	list := s.e.List(engine.ListOptions{All: boolParam(q.Get("all")), Limit: limit, Filters: f})
	out := []map[string]any{}
	for _, c := range list {
		in, err := s.e.Inspect(c.ID)
		if err != nil {
			continue
		}
		pod := in.Config.Labels[podLabel]
		if len(podFilter) > 0 && !s.pods.matches(pod, podFilter) {
			continue
		}
		names := []string{}
		for _, n := range c.Names {
			names = append(names, strings.TrimPrefix(n, "/"))
		}
		var mounts []string
		for _, m := range c.Mounts {
			mounts = append(mounts, m.Destination)
		}
		var networks []string
		if c.NetworkSettings != nil {
			for n := range c.NetworkSettings.Networks {
				networks = append(networks, n)
			}
		}
		sort.Strings(networks)
		st := in.State
		started, _ := time.Parse(time.RFC3339Nano, st.StartedAt)
		finished, _ := time.Parse(time.RFC3339Nano, st.FinishedAt)
		state := string(st.Status)
		exposed := map[uint16][]string{}
		for p := range in.Config.ExposedPorts {
			exposed[p.Num()] = append(exposed[p.Num()], string(p.Proto()))
		}
		ports := map[string][]map[string]string{}
		if in.NetworkSettings != nil {
			for p, bs := range in.NetworkSettings.Ports {
				for _, b := range bs {
					ip := ""
					if b.HostIP.IsValid() {
						ip = b.HostIP.String()
					}
					ports[p.String()] = append(ports[p.String()], map[string]string{"HostIp": ip, "HostPort": b.HostPort})
				}
			}
		}
		created := time.Unix(c.Created, 0)
		out = append(out, map[string]any{
			"AutoRemove": in.HostConfig.AutoRemove, "Command": append(append([]string{}, in.Config.Entrypoint...), in.Config.Cmd...),
			"Created": created, "CreatedAt": created.Format("2006-01-02 15:04:05.999999999 -0700 MST"),
			"Exited": state == "exited", "ExitedAt": finished.Unix(), "ExitCode": st.ExitCode, "ExposedPorts": exposed,
			"Id": c.ID, "Image": c.Image, "ImageID": c.ImageID, "IsInfra": false, "Labels": c.Labels, "Mounts": mounts,
			"Names": names, "Namespaces": map[string]string{}, "Networks": networks, "Pid": st.Pid, "Pod": pod,
			"PodName": s.pods.name(pod), "Ports": libpodPortMappings(ports), "Restarts": in.RestartCount,
			"StartedAt": started.Unix(), "State": state, "Status": c.Status,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// libpodInspect is the container in the shape of Podman's inspect.
func (s *Server) libpodInspect(w http.ResponseWriter, r *http.Request) {
	in, err := s.e.Inspect(r.PathValue("id"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	d := asMap(in)
	st := asMap(in.State)
	if st == nil {
		st = map[string]any{}
	}
	st["OciVersion"] = "1.2.0"
	st["Status"] = string(in.State.Status)
	cfg := asMap(in.Config)
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["Annotations"] = in.HostConfig.Annotations
	cfg["StopSignal"] = in.Config.StopSignal
	if cfg["StopSignal"] == "" {
		cfg["StopSignal"] = "SIGTERM"
	}
	cfg["CreateCommand"] = []string{}
	cfg["Umask"] = "0022"
	cfg["Timezone"] = ""
	host := asMap(in.HostConfig)
	ports := map[string][]map[string]string{}
	nets := map[string]any{}
	if in.NetworkSettings != nil {
		for p, bs := range in.NetworkSettings.Ports {
			ports[p.String()] = []map[string]string{}
			for _, b := range bs {
				ip := ""
				if b.HostIP.IsValid() {
					ip = b.HostIP.String()
				}
				ports[p.String()] = append(ports[p.String()], map[string]string{"HostIp": ip, "HostPort": b.HostPort})
			}
		}
		for n, es := range in.NetworkSettings.Networks {
			nets[n] = asMap(es)
		}
	}
	created, _ := time.Parse(time.RFC3339Nano, str(d["Created"]))
	out := map[string]any{
		"Id": in.ID, "Created": created, "Path": in.Path, "Args": in.Args, "State": st,
		"Image": in.Image, "ImageDigest": "", "ImageName": in.Config.Image, "Rootfs": "",
		"Pod": in.Config.Labels[podLabel], "ResolvConfPath": in.ResolvConfPath, "HostnamePath": in.HostnamePath,
		"HostsPath": in.HostsPath, "StaticDir": "", "ConmonPidFile": "", "PidFile": "",
		"Name": strings.TrimPrefix(in.Name, "/"), "RestartCount": in.RestartCount, "Driver": in.Driver,
		"MountLabel": "", "ProcessLabel": "", "AppArmorProfile": "", "EffectiveCaps": []string{}, "BoundingCaps": []string{},
		"ExecIDs": in.ExecIDs, "GraphDriver": map[string]any{"Name": in.Driver, "Data": map[string]string{}},
		"Mounts": d["Mounts"], "Dependencies": []string{},
		"NetworkSettings": map[string]any{"Ports": ports, "Networks": nets, "SandboxKey": "", "Bridge": ""},
		"Namespace":       "", "IsInfra": false, "IsService": false, "KubeExitCodePropagation": "invalid", "lockNumber": 0,
		"Config": cfg, "HostConfig": host, "UseImageHosts": false, "UseImageHostname": false,
	}
	if out["ExecIDs"] == nil {
		out["ExecIDs"] = []string{}
	}
	if out["Mounts"] == nil {
		out["Mounts"] = []any{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) libpodRemove(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := r.PathValue("id")
	c, err := s.e.Container(id)
	if err != nil {
		if engine.KindOf(err) == engine.KindNotFound && boolParam(q.Get("ignore")) {
			writeJSON(w, http.StatusOK, []map[string]string{})
			return
		}
		writeEngineError(w, err)
		return
	}
	full := c.ID()
	if err := s.e.Remove(id, engine.RemoveOptions{Force: boolParam(q.Get("force")), RemoveVolumes: boolParam(q.Get("v"))}); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, []map[string]string{{"Id": full}})
}

// libpodWait waits for the first of the conditions Podman names: the
// container stopping or being removed, which answers its exit code, or
// running or turning healthy or unhealthy, which answers -1, as Podman's
// does, since the container has none.
func (s *Server) libpodWait(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stop, states, health := false, map[string]bool{}, map[string]bool{}
	cond := container.WaitConditionNotRunning
	for _, c := range r.URL.Query()["condition"] {
		switch c {
		case "stopped", "exited", "":
			stop = true
		case "removing", "removed":
			stop, cond = true, container.WaitConditionRemoved
		case "running", "paused", "created":
			states[c] = true
		case "healthy", "unhealthy":
			health[c] = true
		default:
			writeError(w, http.StatusBadRequest, "invalid condition: "+c)
			return
		}
	}
	if len(states) == 0 && len(health) == 0 {
		stop = true
	}
	c, err := s.e.Container(id)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	full := c.ID()
	// Subscribe before reading the state, so no change falls between.
	_, events, cancel := s.e.Subscribe(time.Time{})
	defer cancel()
	var exited <-chan container.WaitResponse
	if stop {
		if exited, err = s.e.Wait(id, cond); err != nil {
			writeEngineError(w, err)
			return
		}
	}
	reached := func() bool {
		in, err := s.e.Inspect(full)
		if err != nil || in.State == nil {
			return false
		}
		if states[string(in.State.Status)] {
			return true
		}
		return in.State.Health != nil && health[string(in.State.Health.Status)]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flush(w)
	for !reached() {
		// Read the state again on the container's next event.
	next:
		select {
		case res := <-exited:
			json.NewEncoder(w).Encode(int32(res.StatusCode))
			return
		case m, ok := <-events:
			if !ok {
				return
			}
			if m.Actor.ID != full {
				goto next
			}
		case <-r.Context().Done():
			return
		}
	}
	json.NewEncoder(w).Encode(int32(-1))
}
