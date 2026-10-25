package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const serverVersion = "1.0.0"

type Server struct {
	cfg        *Config
	configPath string
	store      *Store
	http       *providerHTTP
	started    time.Time

	runsMu       sync.Mutex
	runs         map[string]*Run
	finishedRuns map[string]*Run
	finishedIDs  []string

	scheduledStore *ScheduledStore
	scheduler      *Scheduler

	configMu      sync.Mutex
	pendingConfig map[string]pendingConfigChange

	updateMu    sync.Mutex
	updateCache map[string]updateCacheEntry
}

type updateCacheEntry struct {
	body      []byte
	etag      string
	expiresAt time.Time
}

func NewServer(cfg *Config, store *Store, configPath string) *Server {
	scheduledStore, err := NewScheduledStore(filepath.Join(cfg.DataDir, "scheduled.json"))
	if err != nil {
		log.Fatalf("open scheduled store: %v", err)
	}
	s := &Server{
		cfg:            cfg,
		configPath:     configPath,
		store:          store,
		http:           newProviderHTTP(),
		started:        time.Now(),
		runs:           map[string]*Run{},
		finishedRuns:   map[string]*Run{},
		updateCache:    map[string]updateCacheEntry{},
		scheduledStore: scheduledStore,
		pendingConfig:  map[string]pendingConfigChange{},
	}
	s.scheduler = NewScheduler(cfg, s, scheduledStore)
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /v1/models", s.auth(http.HandlerFunc(s.handleModels)))
	mux.Handle("POST /v1/chat/completions", s.auth(http.HandlerFunc(s.handleChatCompletions)))
	mux.Handle("POST /v1/agent/runs", s.auth(http.HandlerFunc(s.handleAgentRun)))
	mux.Handle("POST /v1/files/list", s.auth(http.HandlerFunc(s.handleFileList)))
	mux.Handle("GET /v1/files/raw", s.auth(http.HandlerFunc(s.handleFileRaw)))
	mux.Handle("POST /v1/files/upload", s.auth(http.HandlerFunc(s.handleFileUpload)))
	mux.Handle("POST /v1/files/delete", s.auth(http.HandlerFunc(s.handleFileDelete)))
	mux.Handle("POST /v1/files/rename", s.auth(http.HandlerFunc(s.handleFileRename)))
	mux.Handle("POST /v1/files/mkdir", s.auth(http.HandlerFunc(s.handleFileMkdir)))
	mux.Handle("POST /v1/terminal", s.auth(http.HandlerFunc(s.handleTerminalStart)))
	mux.Handle("POST /v1/terminal/{id}/input", s.auth(http.HandlerFunc(s.handleTerminalInput)))
	mux.Handle("POST /v1/terminal/{id}/resize", s.auth(http.HandlerFunc(s.handleTerminalResize)))
	mux.Handle("POST /v1/terminal/{id}/close", s.auth(http.HandlerFunc(s.handleTerminalClose)))
	mux.Handle("GET /v1/scheduled", s.auth(http.HandlerFunc(s.handleScheduledList)))
	mux.Handle("POST /v1/scheduled", s.auth(http.HandlerFunc(s.handleScheduledUpsert)))
	mux.Handle("DELETE /v1/scheduled/{id}", s.auth(http.HandlerFunc(s.handleScheduledDelete)))
	mux.Handle("POST /v1/scheduled/{id}/run", s.auth(http.HandlerFunc(s.handleScheduledRun)))
	mux.Handle("GET /v1/cli/sessions", s.cliAuth(http.HandlerFunc(s.handleCLISessions)))
	mux.Handle("GET /v1/cli/search", s.cliAuth(http.HandlerFunc(s.handleCLISearch)))
	mux.Handle("GET /v1/cli/sessions/{id}/messages", s.cliAuth(http.HandlerFunc(s.handleCLISessionMessages)))
	mux.Handle("POST /v1/cli/sessions/{id}/update", s.cliAuth(http.HandlerFunc(s.handleCLISessionUpdate)))
	mux.Handle("GET /v1/cli/models", s.cliAuth(http.HandlerFunc(s.handleCLIModels)))
	mux.Handle("POST /v1/cli/model-use", s.cliAuth(http.HandlerFunc(s.handleCLIModelUse)))
	mux.Handle("GET /v1/cli/config", s.cliAuth(http.HandlerFunc(s.handleCLIConfigGet)))
	mux.Handle("POST /v1/cli/config", s.cliAuth(http.HandlerFunc(s.handleCLIConfigSet)))
	mux.Handle("POST /v1/cli/config/confirm", s.cliAuth(http.HandlerFunc(s.handleCLIConfigConfirm)))
	mux.Handle("GET /v1/cli/scheduled", s.cliAuth(http.HandlerFunc(s.handleScheduledList)))
	mux.Handle("POST /v1/cli/scheduled", s.cliAuth(http.HandlerFunc(s.handleScheduledUpsert)))
	mux.Handle("DELETE /v1/cli/scheduled/{id}", s.cliAuth(http.HandlerFunc(s.handleScheduledDelete)))
	mux.Handle("POST /v1/cli/scheduled/{id}/run", s.cliAuth(http.HandlerFunc(s.handleScheduledRun)))
	mux.Handle("POST /v1/agent/runs/{id}/tool_result", s.auth(http.HandlerFunc(s.handleToolResult)))
	mux.Handle("POST /v1/agent/runs/{id}/cancel", s.auth(http.HandlerFunc(s.handleCancelRun)))
	mux.Handle("GET /v1/agent/runs/{id}/events", s.auth(http.HandlerFunc(s.handleRunEvents)))
	mux.Handle("POST /v1/agent/runs/{id}/steer", s.auth(http.HandlerFunc(s.handleSteerRun)))
	mux.Handle("GET /v1/sessions", s.auth(http.HandlerFunc(s.handleListSessions)))
	mux.Handle("GET /v1/sessions/{id}", s.auth(http.HandlerFunc(s.handleGetSession)))
	mux.Handle("DELETE /v1/sessions/{id}", s.auth(http.HandlerFunc(s.handleDeleteSession)))
	mux.Handle("POST /v1/sessions/{id}/meta", s.auth(http.HandlerFunc(s.handleSessionMeta)))
	mux.Handle("GET /v1/status", s.auth(http.HandlerFunc(s.handleStatus)))
	mux.Handle("POST /v1/admin/config", s.auth(http.HandlerFunc(s.handleAdminConfig)))
	mux.Handle("POST /v1/admin/provider", s.auth(http.HandlerFunc(s.handleAdminProvider)))
	mux.Handle("POST /v1/admin/update-token", s.auth(http.HandlerFunc(s.handleAdminUpdateToken)))
	mux.Handle("GET /v1/app/releases", s.auth(http.HandlerFunc(s.handleAppReleases)))
	mux.Handle("GET /v1/app/assets/{id}", s.auth(http.HandlerFunc(s.handleAppAsset)))
	mux.HandleFunc("GET /v1/app/releases/{tag}", s.handleReleasePage)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name":    "open-minis-server",
			"version": serverVersion,
			"status":  "ok",
		})
	})
	return s.logRequests(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		token := strings.TrimPrefix(h, "Bearer ")
		if token == h || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AuthToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(lw, r)
		// Do not log query strings: an update check's ETag is harmless, but
		// keeping logs path-only is a simple habit that avoids future leaks.
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, lw.status, time.Since(start).Round(time.Millisecond))
	})
}

type loggingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *loggingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *loggingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"version":    serverVersion,
		"uptime_sec": int64(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   s.cfg.ModelList(),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.runsMu.Lock()
	active := len(s.runs)
	s.runsMu.Unlock()
	models := 0
	for _, p := range s.cfg.Providers {
		models += len(p.Models)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"version":     serverVersion,
		"uptime_sec":  int64(time.Since(s.started).Seconds()),
		"active_runs": active,
		"model_count": models,
		"sessions":    len(s.store.ListSessions()),
		"update_repo": s.cfg.Update.Repo,
	})
}

func (s *Server) handleAgentRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024*1024))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.SessionID) == "" || !sessionIDPattern.MatchString(req.SessionID) {
		writeError(w, http.StatusBadRequest, "session_id is required and may contain letters, digits, _ and -")
		return
	}
	if req.RunID == "" {
		req.RunID = newID("run")
	}
	if !sessionIDPattern.MatchString(req.RunID) {
		writeError(w, http.StatusBadRequest, "run_id is invalid")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages is required")
		return
	}
	if _, _, err := s.cfg.ResolveModel(req.Model); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	run := newRun(context.Background(), req.RunID)
	s.registerRun(run)
	go func() {
		s.runAgent(req, run)
		s.unregisterRun(run)
	}()
	s.streamRun(w, r, run, 0)
}

// streamRun attaches an HTTP response to a run's event log. Disconnecting does
// not cancel the run; the app can reconnect with `after` (or Last-Event-ID)
// and continue from the exact event it missed.
func (s *Server) streamRun(w http.ResponseWriter, r *http.Request, run *Run, after int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this HTTP stack")
		return
	}
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := parseInt(v); err == nil && n > 0 {
			after = n
		}
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := parseInt(v); err == nil && n > after {
			after = n
		}
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	if run.finished() && after >= run.lastSeq() {
		// Nothing left to replay. Emit a terminal event instead of an empty
		// stream the client would reconnect to forever.
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: done\ndata: {\"stop_reason\":\"replay_end\",\"turns\":0}\n\n")
		flusher.Flush()
		return
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	ctx, cancel := context.WithCancel(r.Context())
	done := make(chan struct{})
	var writeMu sync.Mutex
	write := func(format string, args ...any) bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	go func() {
		defer close(done)
		run.subscribe(ctx, after, func(seq int, ev Event) bool {
			data, err := json.Marshal(ev.Data)
			if err != nil {
				data = []byte(`{"error":"unable to encode event"}`)
			}
			return write("id: %d\nevent: %s\ndata: %s\n\n", seq, ev.Name, data)
		})
	}()
	// Do not let the HTTP handler return while the subscriber goroutine may
	// still be writing to the response. net/http then flushes the connection
	// from another goroutine, which is a data race (and can corrupt the tail).
	defer func() {
		cancel()
		<-done
	}()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !write(": ping\n\n") {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.runsMu.Lock()
	run := s.runs[id]
	if run == nil {
		run = s.finishedRuns[id]
	}
	s.runsMu.Unlock()
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	s.streamRun(w, r, run, 0)
}

func (s *Server) handleToolResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var res ToolResult
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024*1024))
	if err := dec.Decode(&res); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if res.ID == "" {
		writeError(w, http.StatusBadRequest, "tool result id is required")
		return
	}
	s.runsMu.Lock()
	run := s.runs[id]
	s.runsMu.Unlock()
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err := run.deliverClientTool(res); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

func (s *Server) handleSteerRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.runsMu.Lock()
	run := s.runs[id]
	s.runsMu.Unlock()
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	var req struct {
		Text     string     `json:"text"`
		Parts    []WirePart `json:"parts"`
		ClientID string     `json:"client_id"`
	}
	if err := jsonDecodeLimited(w, r, &req, 4*1024*1024); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	parts := req.Parts
	if len(parts) == 0 && strings.TrimSpace(req.Text) != "" {
		parts = []WirePart{{Type: "text", Text: req.Text}}
	}
	if len(parts) == 0 {
		writeError(w, http.StatusBadRequest, "text or parts is required")
		return
	}
	run.addSteer(WireMessage{Role: "user", Parts: parts})
	run.emit("steered", map[string]any{"client_id": req.ClientID, "text": req.Text})
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if !s.cancelRun(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"cancelled": true})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.store.ListSessions()})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	meta, err := s.store.GetSession(id)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	msgs, err := s.store.Messages(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": meta, "messages": msgs})
}

func (s *Server) handleSessionMeta(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title *string `json:"title"`
	}
	if err := jsonDecodeLimited(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Title == nil || strings.TrimSpace(*req.Title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if err := s.store.UpdateSessionTitle(r.PathValue("id"), strings.TrimSpace(*req.Title)); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.runsMu.Lock()
	var active []*Run
	for _, run := range s.runs {
		if run.Session == id {
			run.cancel()
			active = append(active, run)
		}
	}
	s.runsMu.Unlock()
	for _, run := range active {
		run.waitDone(5 * time.Second)
	}
	if err := s.store.DeleteSession(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if sb, err := NewToolSandbox(s.cfg.Sandbox, id); err == nil {
		if err := sb.RemoveSession(); err != nil {
			log.Printf("delete session workspace %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "session record deleted, but its workspace could not be removed: "+err.Error())
			return
		}
	} else {
		log.Printf("delete session workspace %s: %v", id, err)
	}
	w.WriteHeader(http.StatusNoContent)
}
