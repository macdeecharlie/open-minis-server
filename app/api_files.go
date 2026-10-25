package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

func (s *Server) fileSandbox(sessionID string) (ToolSandbox, error) {
	return NewToolSandbox(s.cfg.Sandbox, sessionID)
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		Path      string `json:"path"`
	}
	if err := jsonDecodeLimited(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sb, err := s.fileSandbox(req.SessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	entries, err := sb.ListDir(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleFileRaw(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	path := r.URL.Query().Get("path")
	sb, err := s.fileSandbox(sessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	data, contentType, err := sb.ReadRaw(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	path := r.URL.Query().Get("path")
	sb, err := s.fileSandbox(sessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	if err := sb.Upload(path, data); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bytes": len(data)})
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		Path      string `json:"path"`
	}
	if err := jsonDecodeLimited(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sb, err := s.fileSandbox(req.SessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := sb.Delete(req.Path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFileRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		From      string `json:"from"`
		To        string `json:"to"`
	}
	if err := jsonDecodeLimited(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sb, err := s.fileSandbox(req.SessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := sb.Rename(req.From, req.To); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		Path      string `json:"path"`
	}
	if err := jsonDecodeLimited(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sb, err := s.fileSandbox(req.SessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := sb.Mkdir(req.Path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func jsonDecodeLimited(w http.ResponseWriter, r *http.Request, out any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	return dec.Decode(out)
}
