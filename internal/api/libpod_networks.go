package api

import (
	"net/http"
	"net/netip"
	"sort"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

func (s *Server) libpodNetworkRoutes() {
	m := s.lp
	m.HandleFunc("POST /networks/create", s.libpodNetworkCreate)
	m.HandleFunc("GET /networks/json", func(w http.ResponseWriter, r *http.Request) {
		f, err := engine.ParseFilters(r.URL.Query().Get("filters"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		out := []map[string]any{}
		for _, n := range s.e.NetworkList(f) {
			in, err := s.e.NetworkInspect(n.ID)
			if err == nil {
				out = append(out, libpodNetwork(in, false))
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
	m.HandleFunc("GET /networks/{name}/json", func(w http.ResponseWriter, r *http.Request) {
		in, err := s.e.NetworkInspect(r.PathValue("name"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, libpodNetwork(in, true))
	})
	m.HandleFunc("GET /networks/{name}/exists", func(w http.ResponseWriter, r *http.Request) {
		_, err := s.e.NetworkInspect(r.PathValue("name"))
		respondStatus(w, http.StatusNoContent, err)
	})
	m.HandleFunc("DELETE /networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		in, err := s.e.NetworkInspect(r.PathValue("name"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		if boolParam(r.URL.Query().Get("force")) {
			for id := range in.Containers {
				if err := s.e.Remove(id, engine.RemoveOptions{Force: true}); err != nil {
					writeEngineError(w, err)
					return
				}
			}
		}
		if err := s.e.NetworkRemove(in.ID); err != nil {
			writeEngineError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, []map[string]string{{"Name": in.Name}})
	})
	m.HandleFunc("POST /networks/{name}/connect", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Container string   `json:"container"`
			Aliases   []string `json:"aliases"`
			StaticIPs []string `json:"static_ips"`
		}
		if !decode(w, r, &req) {
			return
		}
		es := &network.EndpointSettings{Aliases: req.Aliases}
		for _, ip := range req.StaticIPs {
			a, err := netip.ParseAddr(ip)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid static IP "+ip)
				return
			}
			if es.IPAMConfig == nil {
				es.IPAMConfig = &network.EndpointIPAMConfig{}
			}
			if a.Is4() {
				es.IPAMConfig.IPv4Address = a
			} else {
				es.IPAMConfig.IPv6Address = a
			}
		}
		respondStatus(w, http.StatusOK, s.e.NetworkConnect(r.PathValue("name"), network.ConnectRequest{Container: req.Container, EndpointConfig: es}))
	})
	m.HandleFunc("POST /networks/{name}/disconnect", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Container string `json:"Container"`
			Force     bool   `json:"Force"`
		}
		if !decode(w, r, &req) {
			return
		}
		respondStatus(w, http.StatusOK, s.e.NetworkDisconnect(r.PathValue("name"), req.Container, req.Force))
	})
}

// libpodNetwork is a network in Podman's shape, with its containers when
// inspected.
func libpodNetwork(in network.Inspect, containers bool) map[string]any {
	var subnets []map[string]string
	ipv6 := false
	for _, c := range in.IPAM.Config {
		sn := map[string]string{"subnet": c.Subnet.String()}
		if c.Gateway.IsValid() {
			sn["gateway"] = c.Gateway.String()
		}
		subnets = append(subnets, sn)
		ipv6 = ipv6 || c.Subnet.Addr().Is6()
	}
	driver := in.Driver
	if driver == "" {
		driver = "bridge"
	}
	out := map[string]any{
		"name": in.Name, "id": in.ID, "driver": driver, "network_interface": "", "created": in.Created,
		"subnets": subnets, "ipv6_enabled": ipv6 || in.EnableIPv6, "internal": in.Internal, "dns_enabled": in.Name != "bridge",
		"labels": in.Labels, "options": in.Options, "ipam_options": map[string]string{"driver": "host-local"},
	}
	if containers {
		cs := map[string]any{}
		for id, c := range in.Containers {
			cs[id] = map[string]any{"name": c.Name, "interfaces": map[string]any{}}
		}
		out["containers"] = cs
	}
	return out
}

func (s *Server) libpodNetworkCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string            `json:"name"`
		Driver      string            `json:"driver"`
		Internal    bool              `json:"internal"`
		IPv6Enabled bool              `json:"ipv6_enabled"`
		DNSEnabled  bool              `json:"dns_enabled"`
		Labels      map[string]string `json:"labels"`
		Options     map[string]string `json:"options"`
		Subnets     []struct {
			Subnet  string `json:"subnet"`
			Gateway string `json:"gateway"`
		} `json:"subnets"`
	}
	if !decode(w, r, &req) {
		return
	}
	ipam := &network.IPAM{Driver: "default"}
	for _, sn := range req.Subnets {
		p, err := netip.ParsePrefix(sn.Subnet)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid subnet "+sn.Subnet)
			return
		}
		cfg := network.IPAMConfig{Subnet: p}
		if sn.Gateway != "" {
			if cfg.Gateway, err = netip.ParseAddr(sn.Gateway); err != nil {
				writeError(w, http.StatusBadRequest, "invalid gateway "+sn.Gateway)
				return
			}
		}
		ipam.Config = append(ipam.Config, cfg)
	}
	ipv6 := req.IPv6Enabled
	res, err := s.e.NetworkCreate(engine.NetworkCreate{
		Name: req.Name, Driver: req.Driver, Internal: req.Internal, EnableIPv6: &ipv6, IPAM: ipam,
		Options: req.Options, Labels: req.Labels,
	})
	if err != nil {
		writeEngineError(w, err)
		return
	}
	in, err := s.e.NetworkInspect(res.ID)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, libpodNetwork(in, false))
}

func (s *Server) libpodVolumeRoutes() {
	m := s.lp
	m.HandleFunc("POST /volumes/create", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name           string
			Driver         string
			Label          map[string]string
			Labels         map[string]string
			Options        map[string]string
			IgnoreIfExists bool
		}
		if !decode(w, r, &req) {
			return
		}
		labels := req.Labels
		for k, v := range req.Label {
			if labels == nil {
				labels = map[string]string{}
			}
			labels[k] = v
		}
		if req.Name != "" {
			if v, err := s.e.VolumeInspect(req.Name); err == nil {
				if req.IgnoreIfExists {
					writeJSON(w, http.StatusCreated, libpodVolume(v))
					return
				}
				writeError(w, http.StatusInternalServerError, "volume with name "+req.Name+" already exists: volume already exists")
				return
			}
		}
		v, err := s.e.VolumeCreate(volume.CreateRequest{Name: req.Name, Driver: req.Driver, DriverOpts: req.Options, Labels: labels})
		respondCode(w, http.StatusCreated, libpodVolume(v), err)
	})
	m.HandleFunc("GET /volumes/json", func(w http.ResponseWriter, r *http.Request) {
		f, err := engine.ParseFilters(r.URL.Query().Get("filters"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		out := []map[string]any{}
		vols := s.e.VolumeList(f)
		sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
		for _, v := range vols {
			out = append(out, libpodVolume(v))
		}
		writeJSON(w, http.StatusOK, out)
	})
	m.HandleFunc("GET /volumes/{name}/json", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.e.VolumeInspect(r.PathValue("name"))
		respond(w, libpodVolume(v), err)
	})
	m.HandleFunc("GET /volumes/{name}/exists", func(w http.ResponseWriter, r *http.Request) {
		_, err := s.e.VolumeInspect(r.PathValue("name"))
		respondStatus(w, http.StatusNoContent, err)
	})
	m.HandleFunc("DELETE /volumes/{name}", s.simple(func(r *http.Request) error {
		return s.e.VolumeRemove(r.PathValue("name"), boolParam(r.URL.Query().Get("force")))
	}, http.StatusNoContent))
	m.HandleFunc("POST /volumes/prune", func(w http.ResponseWriter, r *http.Request) {
		f, err := engine.ParseFilters(r.URL.Query().Get("filters"))
		if err != nil {
			writeEngineError(w, err)
			return
		}
		rep, err := s.e.VolumePrune(f)
		out := []map[string]any{}
		for _, n := range rep.VolumesDeleted {
			out = append(out, map[string]any{"Id": n, "Size": 0})
		}
		respond(w, out, err)
	})
}

// libpodVolume is a volume in the shape of Podman's inspect.
func libpodVolume(v volume.Volume) map[string]any {
	opts := v.Options
	if opts == nil {
		opts = map[string]string{}
	}
	labels := v.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	_, anon := v.Labels["com.docker.volume.anonymous"]
	refs := 0
	if v.UsageData != nil {
		refs = int(v.UsageData.RefCount)
	}
	return map[string]any{
		"Name": v.Name, "Driver": v.Driver, "Mountpoint": v.Mountpoint, "CreatedAt": v.CreatedAt, "Labels": labels,
		"Scope": v.Scope, "Options": opts, "UID": 0, "GID": 0, "Anonymous": anon, "MountCount": refs,
		"NeedsCopyUp": true, "NeedsChown": true, "LockNumber": 0,
	}
}
