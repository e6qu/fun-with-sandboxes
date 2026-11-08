package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

func (s *Server) libpodImageRoutes() {
	m := s.lp
	m.HandleFunc("GET /images/json", s.libpodImageList)
	m.HandleFunc("POST /images/pull", s.libpodPull)
	m.HandleFunc("POST /images/load", s.libpodLoad)
	m.HandleFunc("DELETE /images/remove", s.libpodImageRemove)
	m.HandleFunc("GET /images/export", func(w http.ResponseWriter, r *http.Request) { s.save(w, r.URL.Query()["references"]) })
	m.HandleFunc("GET /images/{ref...}", s.libpodImageGet)
	m.HandleFunc("POST /images/{ref...}", s.libpodImagePost)
	m.HandleFunc("DELETE /images/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		r.URL.RawQuery += "&images=" + r.PathValue("ref")
		s.libpodImageRemove(w, r)
	})
}

func (s *Server) libpodImageList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := engine.ParseFilters(q.Get("filters"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if !boolParam(q.Get("all")) {
		f["dangling"] = append(f["dangling"], "false")
	}
	out := []map[string]any{}
	for _, img := range s.e.ImageList(f) {
		in, err := s.e.ImageInspect(img.ID)
		if err != nil {
			continue
		}
		tags := img.RepoTags
		if tags == nil {
			tags = []string{}
		}
		out = append(out, map[string]any{
			"Id": strings.TrimPrefix(img.ID, "sha256:"), "ParentId": img.ParentID, "RepoTags": tags, "RepoDigests": img.RepoDigests,
			"Created": img.Created, "Size": img.Size, "SharedSize": 0, "VirtualSize": img.Size, "Labels": img.Labels,
			"Containers": img.Containers, "Dangling": len(tags) == 0, "Arch": in.Architecture, "Os": in.Os,
			"Digest": firstName(img.RepoDigests), "Names": tags,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// libpodImage is an image in the shape of Podman's inspect.
func (s *Server) libpodImage(ref string) (map[string]any, error) {
	in, err := s.e.ImageInspect(ref)
	if err != nil {
		return nil, err
	}
	d := asMap(in)
	cfg := map[string]any{}
	if in.Config != nil {
		cfg = asMap(in.Config)
	}
	return map[string]any{
		"Id": strings.TrimPrefix(in.ID, "sha256:"), "Digest": firstName(in.RepoDigests), "RepoTags": in.RepoTags, "RepoDigests": in.RepoDigests,
		"Parent": d["Parent"], "Comment": in.Comment, "Created": d["Created"], "Config": cfg, "Version": d["DockerVersion"],
		"Author": in.Author, "Architecture": in.Architecture, "Os": in.Os, "Size": in.Size, "VirtualSize": in.Size,
		"GraphDriver": d["GraphDriver"], "RootFS": d["RootFS"], "Labels": cfg["Labels"], "Annotations": map[string]string{},
		"ManifestType": "application/vnd.oci.image.manifest.v1+json", "User": cfg["User"], "History": []any{},
		"NamesHistory": in.RepoTags, "HealthCheck": cfg["Healthcheck"],
	}, nil
}

func (s *Server) libpodImageGet(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	switch {
	case strings.HasSuffix(ref, "/json"):
		img, err := s.libpodImage(strings.TrimSuffix(ref, "/json"))
		respond(w, img, err)
	case strings.HasSuffix(ref, "/exists"):
		_, err := s.e.ImageInspect(strings.TrimSuffix(ref, "/exists"))
		respondStatus(w, http.StatusNoContent, err)
	case strings.HasSuffix(ref, "/history"):
		res, err := s.e.History(strings.TrimSuffix(ref, "/history"))
		respond(w, res, err)
	case strings.HasSuffix(ref, "/get"):
		s.save(w, []string{strings.TrimSuffix(ref, "/get")})
	default:
		writeError(w, http.StatusNotFound, "page not found")
	}
}

func (s *Server) libpodImagePost(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	q := r.URL.Query()
	switch {
	case strings.HasSuffix(ref, "/tag"):
		err := s.e.ImageTag(strings.TrimSuffix(ref, "/tag"), q.Get("repo"), q.Get("tag"))
		respondStatus(w, http.StatusCreated, err)
	case strings.HasSuffix(ref, "/untag"):
		target := strings.TrimSuffix(ref, "/untag")
		if repo := q.Get("repo"); repo != "" {
			tag := q.Get("tag")
			if tag == "" {
				tag = "latest"
			}
			target = repo + ":" + tag
		}
		_, err := s.e.ImageRemove(target, false)
		respondStatus(w, http.StatusCreated, err)
	case strings.HasSuffix(ref, "/push"):
		dest := q.Get("destination")
		if dest == "" {
			dest = strings.TrimSuffix(ref, "/push")
		}
		repo, tag := dest, ""
		if i := strings.LastIndex(dest, ":"); i > strings.LastIndex(dest, "/") {
			repo, tag = dest[:i], dest[i+1:]
		}
		if src := strings.TrimSuffix(ref, "/push"); src != dest {
			if err := s.e.ImageTag(src, repo, tag); err != nil {
				writeEngineError(w, err)
				return
			}
		}
		s.push(w, r, repo)
	default:
		writeError(w, http.StatusNotFound, "page not found")
	}
}

// libpodPull pulls as podman pull does: a stream of progress lines, then
// the IDs of the images it pulled.
func (s *Server) libpodPull(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ref := q.Get("reference")
	if ref == "" {
		writeError(w, http.StatusBadRequest, "reference parameter cannot be empty")
		return
	}
	auth, err := registryAuth(r.Header.Get("X-Registry-Auth"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if a := q.Get("arch"); a != "" && a != image.Platform.Architecture {
		writeError(w, http.StatusNotImplemented, "requested architecture "+a+" does not match the engine's, "+image.Platform.Architecture)
		return
	}
	if o := q.Get("OS"); o != "" && o != image.Platform.OS {
		writeError(w, http.StatusNotImplemented, "requested OS "+o+" does not match the engine's, "+image.Platform.OS)
		return
	}
	if q.Get("policy") == "never" {
		if in, err := s.e.ImageInspect(ref); err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"images": []string{strings.TrimPrefix(in.ID, "sha256:")}, "id": strings.TrimPrefix(in.ID, "sha256:")})
		} else {
			writeEngineError(w, err)
		}
		return
	}
	if q.Get("policy") == "missing" {
		if in, err := s.e.ImageInspect(ref); err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"images": []string{strings.TrimPrefix(in.ID, "sha256:")}, "id": strings.TrimPrefix(in.ID, "sha256:")})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	quiet := boolParam(q.Get("quiet"))
	pulled, err := s.e.Images().Pull(r.Context(), ref, auth, func(id, status string) {
		if !quiet {
			enc.Encode(map[string]string{"stream": status + " " + id + "\n"})
			flush(w)
		}
	})
	if err != nil {
		enc.Encode(map[string]string{"error": err.Error()})
		return
	}
	in, err := s.e.ImageInspect(pulled.Ref)
	if err != nil {
		enc.Encode(map[string]string{"error": err.Error()})
		return
	}
	id := strings.TrimPrefix(in.ID, "sha256:")
	if !quiet {
		enc.Encode(map[string]string{"stream": "Writing manifest to image destination\n"})
	}
	enc.Encode(map[string]any{"images": []string{id}, "id": id})
}

func (s *Server) libpodLoad(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.e.Images().Load(r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	names := []string{}
	for _, img := range imgs {
		if len(img.RepoTags) == 0 {
			names = append(names, "sha256:"+strings.TrimPrefix(img.ID, "sha256:"))
		}
		names = append(names, img.RepoTags...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"Names": names})
}

// libpodImageRemove removes the images named, and reports as podman rmi
// does: what it untagged and deleted, and an exit code.
func (s *Server) libpodImageRemove(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	force := boolParam(q.Get("force"))
	refs := q["images"]
	if boolParam(q.Get("all")) {
		for _, img := range s.e.ImageList(engine.Filters{}) {
			refs = append(refs, img.ID)
		}
	}
	report := map[string]any{"Deleted": []string{}, "Untagged": []string{}, "ExitCode": 0}
	var deleted, untagged []string
	for _, ref := range refs {
		res, err := s.e.ImageRemove(ref, force)
		if err != nil {
			if engine.KindOf(err) == engine.KindNotFound && boolParam(q.Get("ignore")) {
				continue
			}
			writeEngineError(w, err)
			return
		}
		for _, d := range res {
			if d.Untagged != "" {
				untagged = append(untagged, d.Untagged)
			}
			if d.Deleted != "" {
				deleted = append(deleted, strings.TrimPrefix(d.Deleted, "sha256:"))
			}
		}
	}
	if deleted != nil {
		report["Deleted"] = deleted
	}
	if untagged != nil {
		report["Untagged"] = untagged
	}
	writeJSON(w, http.StatusOK, report)
}
