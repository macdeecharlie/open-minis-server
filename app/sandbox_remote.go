package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// remoteSandbox talks to the dedicated full-Linux sandbox container. The
// server keeps provider keys and the agent loop; the sandbox keeps the distro,
// the shell and the session filesystems.
type remoteSandbox struct {
	cfg     SandboxConfig
	session string
	baseURL string
	token   string
	client  *http.Client
}

func NewRemoteSandbox(cfg SandboxConfig, sessionID string) (ToolSandbox, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return nil, fmt.Errorf("invalid session id")
	}
	if strings.TrimSpace(cfg.AgentURL) == "" {
		return nil, fmt.Errorf("sandbox.agent_url is required in agent mode")
	}
	if strings.TrimSpace(cfg.AgentToken) == "" {
		return nil, fmt.Errorf("sandbox agent token is required in agent mode")
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	return &remoteSandbox{
		cfg:     cfg,
		session: sessionID,
		baseURL: strings.TrimSuffix(cfg.AgentURL, "/"),
		token:   cfg.AgentToken,
		client:  &http.Client{Transport: transport},
	}, nil
}

func (r *remoteSandbox) Workspace() string { return "/var/minis/workspace" }

func (r *remoteSandbox) postJSON(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return fmt.Errorf("sandbox HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r *remoteSandbox) RunShell(ctx context.Context, command string, timeout time.Duration, delay time.Duration, emit func(string)) (ShellResult, error) {
	timeoutSec := int(timeout / time.Second)
	if timeoutSec <= 0 {
		timeoutSec = 900
	}
	delaySec := int(delay / time.Second)
	if delaySec < 0 {
		delaySec = 0
	}
	body := map[string]any{
		"session_id":  r.session,
		"command":     command,
		"timeout_sec": timeoutSec,
		"delay_sec":   delaySec,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ShellResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/v1/exec", bytes.NewReader(raw))
	if err != nil {
		return ShellResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return ShellResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return ShellResult{}, fmt.Errorf("sandbox exec HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var result *ShellResult
	var lines strings.Builder
	err = readSSE(resp.Body, func(event string, data []byte) error {
		switch event {
		case "output":
			var ev struct {
				Line string `json:"line"`
			}
			if err := json.Unmarshal(data, &ev); err != nil {
				return err
			}
			lines.WriteString(ev.Line)
			lines.WriteByte('\n')
			if emit != nil {
				emit(ev.Line)
			}
		case "result":
			var ev struct {
				ExitCode int    `json:"exit_code"`
				Output   string `json:"output"`
				TimedOut bool   `json:"timed_out"`
			}
			if err := json.Unmarshal(data, &ev); err != nil {
				return err
			}
			result = &ShellResult{ExitCode: ev.ExitCode, Output: ev.Output, TimedOut: ev.TimedOut}
		case "error":
			var ev struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(data, &ev)
			if ev.Message == "" {
				ev.Message = "sandbox execution failed"
			}
			return fmt.Errorf("%s", ev.Message)
		}
		return nil
	})
	if err != nil {
		return ShellResult{}, err
	}
	if result == nil {
		return ShellResult{}, fmt.Errorf("sandbox exec ended without a result")
	}
	if result.Output == "" && lines.Len() > 0 {
		result.Output = lines.String()
	}
	return *result, nil
}

func (r *remoteSandbox) ReadFile(p string, offset int, lines int, maxLength int) (string, error) {
	return r.ReadFileDirection(p, offset, lines, maxLength, "head")
}

func (r *remoteSandbox) ReadFileDirection(p string, offset int, lines int, maxLength int, direction string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var out struct {
		Content string `json:"content"`
	}
	err := r.postJSON(ctx, "/v1/files/read", map[string]any{
		"session_id": r.session, "path": p, "offset": offset, "lines": lines,
		"max_length": maxLength, "direction": direction,
	}, &out)
	if err != nil {
		return "", err
	}
	return out.Content, nil
}

func (r *remoteSandbox) WriteFile(p, content string, appendMode bool, createDirs bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return r.postJSON(ctx, "/v1/files/write", map[string]any{
		"session_id": r.session, "path": p, "content": content, "append": appendMode, "create_dirs": createDirs,
	}, nil)
}

func (r *remoteSandbox) ListDir(p string) ([]FileEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var out struct {
		Entries []FileEntry `json:"entries"`
	}
	err := r.postJSON(ctx, "/v1/files/list", map[string]any{
		"session_id": r.session, "path": p,
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.Entries, nil
}

func (r *remoteSandbox) ReadRaw(p string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	body := map[string]any{"session_id": r.session, "path": p}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/v1/files/raw", bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return nil, "", fmt.Errorf("sandbox HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
	if err != nil {
		return nil, "", err
	}
	return data, resp.Header.Get("Content-Type"), nil
}

func (r *remoteSandbox) Upload(p string, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	q := url.Values{}
	q.Set("session_id", r.session)
	q.Set("path", p)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/v1/files/upload?"+q.Encode(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return fmt.Errorf("sandbox HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (r *remoteSandbox) Delete(p string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return r.postJSON(ctx, "/v1/files/delete", map[string]any{"session_id": r.session, "path": p}, nil)
}

func (r *remoteSandbox) Rename(from, to string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return r.postJSON(ctx, "/v1/files/rename", map[string]any{"session_id": r.session, "from": from, "to": to}, nil)
}

func (r *remoteSandbox) Mkdir(p string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return r.postJSON(ctx, "/v1/files/mkdir", map[string]any{"session_id": r.session, "path": p}, nil)
}

func (r *remoteSandbox) RemoveSession() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return r.postJSON(ctx, "/v1/session/delete", map[string]any{"session_id": r.session}, nil)
}

func (r *remoteSandbox) EditFile(p, oldString, newString string, replaceAll bool) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var out struct {
		Replacements int `json:"replacements"`
	}
	err := r.postJSON(ctx, "/v1/files/edit", map[string]any{
		"session_id": r.session, "path": p, "old_string": oldString, "new_string": newString, "replace_all": replaceAll,
	}, &out)
	if err != nil {
		return 0, err
	}
	return out.Replacements, nil
}
