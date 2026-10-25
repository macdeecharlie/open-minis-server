package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// proxySandbox forwards a request to the dedicated sandbox container. The app
// never sees the sandbox token or address.
func (s *Server) proxySandbox(w http.ResponseWriter, r *http.Request, path string) {
	base := strings.TrimSuffix(s.cfg.Sandbox.AgentURL, "/")
	if base == "" {
		writeError(w, http.StatusServiceUnavailable, "sandbox agent is not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, base+path, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Sandbox.AgentToken)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", r.Header.Get("Accept"))
	resp, err := s.http.client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "sandbox request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
	} else {
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

func (s *Server) handleTerminalStart(w http.ResponseWriter, r *http.Request) {
	s.proxySandbox(w, r, "/v1/terminal")
}

func (s *Server) handleTerminalInput(w http.ResponseWriter, r *http.Request) {
	s.proxySandbox(w, r, "/v1/terminal/"+r.PathValue("id")+"/input")
}

func (s *Server) handleTerminalResize(w http.ResponseWriter, r *http.Request) {
	s.proxySandbox(w, r, "/v1/terminal/"+r.PathValue("id")+"/resize")
}

func (s *Server) handleTerminalClose(w http.ResponseWriter, r *http.Request) {
	s.proxySandbox(w, r, "/v1/terminal/"+r.PathValue("id")+"/close")
}
