package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/moby/moby/api/types/container"
)

// pathStatHeader carries a path's stat to `docker cp`, base64-encoded JSON.
const pathStatHeader = "X-Docker-Container-Path-Stat"

func setPathStat(w http.ResponseWriter, st container.PathStat) {
	b, _ := json.Marshal(st)
	w.Header().Set(pathStatHeader, base64.StdEncoding.EncodeToString(b))
}

func (s *Server) archiveHead(w http.ResponseWriter, r *http.Request) {
	st, err := s.e.ArchiveStat(r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	setPathStat(w, st)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) archiveGet(w http.ResponseWriter, r *http.Request) {
	started := false
	err := s.e.ArchiveGet(r.PathValue("id"), r.URL.Query().Get("path"), func(st container.PathStat) {
		setPathStat(w, st)
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		started = true
	}, w)
	if err != nil && !started {
		writeEngineError(w, err)
	}
}

func (s *Server) archivePut(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	err := s.e.ArchivePut(r.PathValue("id"), q.Get("path"), boolParam(q.Get("noOverwriteDirNonDir")), r.Body)
	respondStatus(w, http.StatusOK, err)
}
