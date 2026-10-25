package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// The sandbox image runs this same binary with -role sandbox. It is a full
// Debian/Ubuntu userland with root-owned shell access; the only credentials it
// holds are its own API bearer token. The server container holds the model and
// update secrets and talks to this API over a private Docker network.
type sandboxAgent struct {
	token     string
	workRoot  string
	maxOutput int
}

func runSandboxAgent() {
	token := strings.TrimSpace(os.Getenv("MINIS_SANDBOX_TOKEN"))
	if token == "" {
		log.Fatal("MINIS_SANDBOX_TOKEN is required in sandbox role")
	}
	workRoot := strings.TrimSpace(os.Getenv("MINIS_SANDBOX_WORK_ROOT"))
	if workRoot == "" {
		workRoot = "/workspace/sessions"
	}
	maxOutput := defaultMaxOutput
	if v := strings.TrimSpace(os.Getenv("MINIS_SANDBOX_MAX_OUTPUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxOutput = n
		}
	}
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		log.Fatalf("create sandbox work root: %v", err)
	}
	agent := &sandboxAgent{token: token, workRoot: workRoot, maxOutput: maxOutput}
	listen := strings.TrimSpace(os.Getenv("MINIS_SANDBOX_LISTEN"))
	if listen == "" {
		listen = ":8090"
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           agent.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("minis-sandbox listening on %s (work_root=%s)", listen, workRoot)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("sandbox listen: %v", err)
	}
}

func (a *sandboxAgent) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.Handle("GET /v1/info", a.auth(http.HandlerFunc(a.info)))
	mux.Handle("POST /v1/exec", a.auth(http.HandlerFunc(a.exec)))
	mux.Handle("POST /v1/files/read", a.auth(http.HandlerFunc(a.readFile)))
	mux.Handle("POST /v1/files/write", a.auth(http.HandlerFunc(a.writeFile)))
	mux.Handle("POST /v1/files/edit", a.auth(http.HandlerFunc(a.editFile)))
	mux.Handle("POST /v1/files/list", a.auth(http.HandlerFunc(a.listDir)))
	mux.Handle("POST /v1/files/raw", a.auth(http.HandlerFunc(a.rawFile)))
	mux.Handle("POST /v1/files/upload", a.auth(http.HandlerFunc(a.uploadFile)))
	return mux
}

func (a *sandboxAgent) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		token := strings.TrimPrefix(h, "Bearer ")
		if token == h || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(a.token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func distroName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return "unknown"
}

func (a *sandboxAgent) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"distro":  distroName(),
		"version": serverVersion,
	})
}

func (a *sandboxAgent) info(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"distro":       distroName(),
		"work_root":    a.workRoot,
		"max_output":   a.maxOutput,
		"capabilities": []string{"shell", "files", "full-distro"},
	})
}

type sandboxRequest struct {
	SessionID string `json:"session_id"`
}

func (a *sandboxAgent) sandboxFor(req sandboxRequest) (*Sandbox, error) {
	if !sessionIDPattern.MatchString(req.SessionID) {
		return nil, fmt.Errorf("invalid session id")
	}
	return NewSandbox(SandboxConfig{
		WorkRoot:  a.workRoot,
		MaxOutput: a.maxOutput,
		// No ShellUser: commands run as root inside this dedicated container,
		// which is the point of the full-distro execution environment.
	}, req.SessionID)
}

func (a *sandboxAgent) exec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		sandboxRequest
		Command    string `json:"command"`
		TimeoutSec int    `json:"timeout_sec"`
		DelaySec   int    `json:"delay_sec"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*1024*1024))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	sb, err := a.sandboxFor(req.sandboxRequest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	timeout := time.Duration(req.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	if timeout > time.Hour {
		timeout = time.Hour
	}
	delay := time.Duration(req.DelaySec) * time.Second
	if delay < 0 {
		delay = 0
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	lines := make(chan string, 256)
	done := make(chan struct{})
	var result ShellResult
	var runErr error
	go func() {
		defer close(done)
		result, runErr = sb.RunShell(ctx, req.Command, timeout, delay, func(line string) {
			select {
			case lines <- line:
			case <-ctx.Done():
			}
		})
	}()

	writeEvent := func(name string, data any) bool {
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for {
		select {
		case line, ok := <-lines:
			if ok {
				if !writeEvent("output", map[string]any{"line": line}) {
					return
				}
			}
		case <-done:
			// Drain any buffered lines first.
			for {
				select {
				case line, ok := <-lines:
					if !ok {
						goto finished
					}
					if !writeEvent("output", map[string]any{"line": line}) {
						return
					}
				default:
					goto finished
				}
			}
		case <-ctx.Done():
			return
		}
	}
finished:
	if runErr != nil {
		if ctx.Err() != nil {
			return
		}
		writeEvent("error", map[string]any{"message": runErr.Error()})
		return
	}
	writeEvent("result", map[string]any{
		"exit_code": result.ExitCode,
		"output":    result.Output,
		"timed_out": result.TimedOut,
	})
}

func (a *sandboxAgent) readFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		sandboxRequest
		Path      string `json:"path"`
		Offset    int    `json:"offset"`
		Lines     int    `json:"lines"`
		MaxLength int    `json:"max_length"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1*1024*1024))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	sb, err := a.sandboxFor(req.sandboxRequest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	content, err := sb.ReadFile(req.Path, req.Offset, req.Lines, req.MaxLength)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": content})
}

func (a *sandboxAgent) writeFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		sandboxRequest
		Path       string `json:"path"`
		Content    string `json:"content"`
		Append     bool   `json:"append"`
		CreateDirs bool   `json:"create_dirs"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024*1024))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if len(req.Content) > 8*1024*1024 {
		writeError(w, http.StatusBadRequest, "content exceeds the 8 MB sandbox limit")
		return
	}
	sb, err := a.sandboxFor(req.sandboxRequest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := sb.WriteFile(req.Path, req.Content, req.Append, req.CreateDirs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *sandboxAgent) listDir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		sandboxRequest
		Path string `json:"path"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1*1024*1024))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	sb, err := a.sandboxFor(req.sandboxRequest)
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

func (a *sandboxAgent) rawFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		sandboxRequest
		Path string `json:"path"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1*1024*1024))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	sb, err := a.sandboxFor(req.sandboxRequest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	data, contentType, err := sb.ReadRaw(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *sandboxAgent) uploadFile(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	path := r.URL.Query().Get("path")
	sb, err := a.sandboxFor(sandboxRequest{SessionID: sessionID})
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

func (a *sandboxAgent) editFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		sandboxRequest
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024*1024))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	sb, err := a.sandboxFor(req.sandboxRequest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := sb.EditFile(req.Path, req.OldString, req.NewString, req.ReplaceAll)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"replacements": n})
}
