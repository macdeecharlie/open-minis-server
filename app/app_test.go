package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T, kind string) *Config {
	t.Helper()
	dir := t.TempDir()
	return &Config{
		AuthToken:    "test-token",
		DataDir:      dir,
		DefaultModel: "mock-1",
		Providers: []Provider{{
			ID:   "mock",
			Kind: kind,
			Models: []Model{{
				ID: "mock-1", Name: "Mock", ContextWindow: 128000, MaxOutputTokens: 4096,
			}},
		}},
		Sandbox: SandboxConfig{WorkRoot: dir + "/workspaces", MaxOutput: 200000},
	}
}

func testServer(t *testing.T, kind string) (*Server, *httptest.Server) {
	t.Helper()
	cfg := testConfig(t, kind)
	store, err := NewStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

type sseFrame struct {
	Event string
	Data  map[string]any
}

func runSSE(t *testing.T, ts *httptest.Server, req RunRequest) []sseFrame {
	t.Helper()
	body, _ := json.Marshal(req)
	hreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs", bytes.NewReader(body))
	hreq.Header.Set("Authorization", "Bearer test-token")
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("run HTTP %d: %s", resp.StatusCode, b)
	}
	var frames []sseFrame
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var m map[string]any
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				t.Fatalf("bad SSE data %q: %v", data, err)
			}
			frames = append(frames, sseFrame{Event: event, Data: m})
			event = ""
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return frames
}

func TestAuthRequired(t *testing.T) {
	_, ts := testServer(t, "mock")
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
}

func TestModels(t *testing.T) {
	_, ts := testServer(t, "mock")
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 1 || payload.Data[0]["id"] != "mock-1" {
		t.Fatalf("unexpected models: %+v", payload.Data)
	}
	if payload.Data[0]["context_window"] != float64(128000) {
		t.Fatalf("missing context window: %+v", payload.Data[0])
	}
}

func TestMockAgentShellTool(t *testing.T) {
	s, ts := testServer(t, "mock")
	req := RunRequest{
		SessionID: "sess-shell",
		RunID:     "run-shell",
		Model:     "mock-1",
		Messages: []WireMessage{{
			Role:  "user",
			Parts: []WirePart{{Type: "text", Text: "please [[shell:echo hello-from-server]]"}},
		}},
	}
	frames := runSSE(t, ts, req)
	var sawToolResult, sawDone bool
	var toolContent string
	for _, f := range frames {
		switch f.Event {
		case "tool_result":
			sawToolResult = true
			toolContent, _ = f.Data["content"].(string)
			if !strings.Contains(toolContent, "hello-from-server") {
				t.Fatalf("tool result missing output: %q", toolContent)
			}
			if f.Data["executor"] != "server" {
				t.Fatalf("expected server executor, got %v", f.Data["executor"])
			}
		case "done":
			sawDone = true
		case "error":
			t.Fatalf("unexpected error: %v", f.Data)
		}
	}
	if !sawToolResult || !sawDone {
		t.Fatalf("missing events: tool=%v done=%v frames=%+v", sawToolResult, sawDone, frames)
	}
	msgs, err := s.store.Messages("sess-shell")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 3 {
		t.Fatalf("expected user, assistant, tool result rows; got %d", len(msgs))
	}
}

func TestClientToolRoundTrip(t *testing.T) {
	s, ts := testServer(t, "mock")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	body, _ := json.Marshal(RunRequest{
		SessionID: "sess-client",
		RunID:     "run-client",
		Model:     "mock-1",
		Messages: []WireMessage{{
			Role:  "user",
			Parts: []WirePart{{Type: "text", Text: "please [[client:memory_get]]"}},
		}},
	})
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/agent/runs", bytes.NewReader(body))
	hreq.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	event := ""
	var toolID string
	var sawResult, sawDone bool
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &m)
		if event == "tool_call" && m["executor"] == "client" {
			toolID, _ = m["id"].(string)
		}
		if event == "await_client" {
			if toolID == "" {
				t.Fatal("await_client before tool_call")
			}
			payload, _ := json.Marshal(ToolResult{ID: toolID, Name: "memory_get", Content: "client says hi", Title: "Memory"})
			preq, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/agent/runs/run-client/tool_result", bytes.NewReader(payload))
			preq.Header.Set("Authorization", "Bearer test-token")
			pres, err := http.DefaultClient.Do(preq)
			if err != nil {
				t.Fatal(err)
			}
			pres.Body.Close()
			if pres.StatusCode != http.StatusAccepted {
				t.Fatalf("tool result HTTP %d", pres.StatusCode)
			}
		}
		if event == "tool_result" && m["executor"] == "client" {
			sawResult = true
			if m["content"] != "client says hi" {
				t.Fatalf("client result content: %v", m["content"])
			}
		}
		if event == "done" {
			sawDone = true
			break
		}
		if event == "error" {
			t.Fatalf("run error: %v", m)
		}
		event = ""
	}
	if !sawResult || !sawDone {
		t.Fatalf("client tool flow incomplete: result=%v done=%v", sawResult, sawDone)
	}
	msgs, _ := s.store.Messages("sess-client")
	if len(msgs) < 3 {
		t.Fatalf("expected stored rows, got %d", len(msgs))
	}
}

func TestSandboxConfinesPaths(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(SandboxConfig{WorkRoot: dir, MaxOutput: 1000}, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.virtualPath("/var/minis/workspace/a.txt"); err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	if _, err := sb.virtualPath("/var/minis/workspace/../../etc/passwd"); err == nil {
		t.Fatal("escape path accepted")
	}
	if _, err := sb.virtualPath("/etc/passwd"); err == nil {
		t.Fatal("host path accepted")
	}
	if err := sb.WriteFile("/var/minis/workspace/nested/a.txt", "hi", false, true); err != nil {
		t.Fatal(err)
	}
	got, err := sb.ReadFile("/var/minis/workspace/nested/a.txt", 1, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "hi") {
		t.Fatalf("read did not return content: %q", got)
	}
}

func TestOpenAIProviderParsesToolsAndEOF(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell_execute","arguments":"{\"command\":\"echo hi\"}"}}]},"finish_reason":null}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		io.WriteString(w, `data: {"usage":{"prompt_tokens":10,"completion_tokens":4}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()
	p := &openAIProvider{cfg: &Provider{ID: "x", Kind: "openai", BaseURL: ts.URL, APIKey: "key", Models: []Model{{ID: "m"}}}, http: newProviderHTTP()}
	var text strings.Builder
	var calls []ProviderEvent
	res, err := p.Stream(context.Background(), ProviderRequest{
		Model:    &Model{ID: "m"},
		Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "hi"}}}},
	}, func(ev ProviderEvent) error {
		switch ev.Kind {
		case "text":
			text.WriteString(ev.Text)
		case "tool_call":
			calls = append(calls, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "Hi" {
		t.Fatalf("text = %q", text.String())
	}
	if len(calls) != 1 || calls[0].Name != "shell_execute" || !strings.Contains(calls[0].Args, "echo hi") {
		t.Fatalf("calls = %+v", calls)
	}
	if res.StopReason != "tool_use" || res.Usage.InputTokens != 10 {
		t.Fatalf("result = %+v", res)
	}
}

func TestChatProxyRewritesAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Errorf("upstream auth = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"up-1"`) {
			t.Errorf("body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	cfg := testConfig(t, "openai")
	cfg.Providers[0].BaseURL = upstream.URL
	cfg.Providers[0].APIKey = "provider-key"
	cfg.Providers[0].Models[0].ID = "up-1"
	cfg.DefaultModel = "up-1"
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"up-1","messages":[]}`))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
	}
}

func TestSandboxRejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(SandboxConfig{WorkRoot: dir, MaxOutput: 1000}, "sess-symlink")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(sb.Workspace(), "leak")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if _, err := sb.ReadFile("/var/minis/workspace/leak", 1, 0, 100); err == nil {
		t.Fatal("symlink to /etc/passwd was accepted")
	}
}

func TestBuildToolsIncludesClientTools(t *testing.T) {
	s := &Server{cfg: &Config{}}
	defs := []ToolDef{
		{Type: "function", Function: ToolFunction{Name: "shell_execute", Description: "server", Parameters: []byte(`{"type":"object"}`)}},
		{Type: "function", Function: ToolFunction{Name: "browser_use", Description: "client", Parameters: []byte(`{"type":"object"}`)}},
		{Type: "function", Function: ToolFunction{Name: "device_ping", Description: "client", Parameters: []byte(`{"type":"object"}`)}},
	}
	out := s.buildTools(defs)
	byName := map[string]string{}
	for _, d := range out {
		byName[d.Function.Name] = d.Function.Description
	}
	if byName["browser_use"] != "client" || byName["device_ping"] != "client" {
		t.Fatalf("client tools missing: %+v", byName)
	}
	if !strings.Contains(byName["shell_execute"], "Debian") {
		t.Fatalf("server shell description not corrected: %+v", byName)
	}
}

func TestClientOwnedFilePaths(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"file_read", `{"path":"/var/minis/attachments/a.png"}`, true},
		{"file_read", `{"path":"/var/minis/skills/x/SKILL.md"}`, true},
		{"file_write", `{"path":"/var/minis/attachments/out.txt"}`, true},
		{"file_edit", `{"path":"/var/minis/skills/x/SKILL.md"}`, true},
		{"file_read", `{"path":"/var/minis/workspace/a.txt"}`, false},
		{"file_read", `{"path":"/tmp/a.txt"}`, false},
		{"shell_execute", `{"command":"cat /var/minis/attachments/a"}`, false},
	}
	for _, c := range cases {
		if got := clientOwnedCall(c.name, []byte(c.args)); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.name, c.args, got, c.want)
		}
	}
}

func TestModelListDefaultFirst(t *testing.T) {
	c := &Config{
		DefaultModel: "b",
		Providers: []Provider{
			{ID: "p1", Kind: "mock", Models: []Model{{ID: "a"}, {ID: "b"}}},
		},
	}
	list := c.ModelList()
	if len(list) != 2 || list[0]["id"] != "b" || list[1]["id"] != "a" {
		t.Fatalf("default not first: %+v", list)
	}
}

func TestRemoteSandboxRoundTrip(t *testing.T) {
	root := t.TempDir()
	agent := &sandboxAgent{token: "sandbox-token", workRoot: root, maxOutput: 200_000}
	ts := httptest.NewServer(agent.handler())
	defer ts.Close()

	sb, err := NewRemoteSandbox(SandboxConfig{
		Mode: "agent", AgentURL: ts.URL, AgentToken: "sandbox-token",
	}, "sess-remote")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := sb.RunShell(ctx, "echo remote-hello", 20*time.Second, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "remote-hello") {
		t.Fatalf("shell result = %+v", res)
	}
	if err := sb.WriteFile("/var/minis/workspace/a.txt", "one\n", false, true); err != nil {
		t.Fatal(err)
	}
	got, err := sb.ReadFile("/var/minis/workspace/a.txt", 1, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "one") {
		t.Fatalf("read = %q", got)
	}
	n, err := sb.EditFile("/var/minis/workspace/a.txt", "one", "two", false)
	if err != nil || n != 1 {
		t.Fatalf("edit n=%d err=%v", n, err)
	}
	got, _ = sb.ReadFile("/var/minis/workspace/a.txt", 1, 0, 1000)
	if !strings.Contains(got, "two") {
		t.Fatalf("read after edit = %q", got)
	}
	entries, err := sb.ListDir("/var/minis/workspace")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "a.txt" && !e.IsDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListDir missing a.txt: %+v", entries)
	}
	raw, _, err := sb.ReadRaw("/var/minis/workspace/a.txt")
	if err != nil || !strings.Contains(string(raw), "two") {
		t.Fatalf("ReadRaw = %q err=%v", raw, err)
	}
	if err := sb.Upload("/var/minis/workspace/b.bin", []byte{0, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	raw, _, err = sb.ReadRaw("/var/minis/workspace/b.bin")
	if err != nil || len(raw) != 4 {
		t.Fatalf("uploaded raw = %v err=%v", raw, err)
	}

	bad, err := NewRemoteSandbox(SandboxConfig{
		Mode: "agent", AgentURL: ts.URL, AgentToken: "wrong",
	}, "sess-remote")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.RunShell(ctx, "echo nope", time.Second, 0, nil); err == nil {
		t.Fatal("wrong sandbox token was accepted")
	}
}

func TestSandboxAgentHealth(t *testing.T) {
	agent := &sandboxAgent{token: "t", workRoot: t.TempDir(), maxOutput: 1000}
	ts := httptest.NewServer(agent.handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health HTTP %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/info", nil)
	req.Header.Set("Authorization", "Bearer t")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var info map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info["capabilities"] == nil {
		t.Fatalf("info = %+v", info)
	}
}

func TestSandboxTerminalPTY(t *testing.T) {
	agent := &sandboxAgent{
		token: "t", workRoot: t.TempDir(), maxOutput: 10000,
		terminals: newTerminalManager(),
	}
	ts := httptest.NewServer(agent.handler())
	defer ts.Close()

	body := strings.NewReader(`{"session_id":"sess-pty","cols":100,"rows":30}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/terminal", body)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("terminal HTTP %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	event := ""
	termID := ""
	var output strings.Builder
	deadline := time.Now().Add(20 * time.Second)
	sent := false
	for sc.Scan() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for PTY output")
		}
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var data map[string]any
		_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &data)
		if event == "ready" {
			termID, _ = data["id"].(string)
			if termID == "" {
				t.Fatal("terminal ready without id")
			}
			ireq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/terminal/"+termID+"/input", strings.NewReader("echo pty-ok\r"))
			ireq.Header.Set("Authorization", "Bearer t")
			iresp, err := http.DefaultClient.Do(ireq)
			if err != nil {
				t.Fatal(err)
			}
			iresp.Body.Close()
			sent = true
		}
		if event == "output" {
			encoded, _ := data["data"].(string)
			b, _ := base64.StdEncoding.DecodeString(encoded)
			output.Write(b)
			if sent && strings.Contains(output.String(), "pty-ok") {
				break
			}
		}
	}
	if !sent || !strings.Contains(output.String(), "pty-ok") {
		t.Fatalf("PTY did not return command output: %q", output.String())
	}
}

func TestScheduledHeadlessRun(t *testing.T) {
	cfg := testConfig(t, "mock")
	store, err := NewStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	task := &ScheduledTask{
		ID: "task_test", Label: "scheduled", Prompt: "run [[shell:echo scheduled-ok]]",
		Target: "new", Enabled: true, Kind: "interval", IntervalSec: 60,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sid, text, err := s.runScheduled(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "scheduled-ok") {
		t.Fatalf("scheduled text = %q", text)
	}
	msgs, err := s.store.Messages(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 3 {
		t.Fatalf("scheduled messages = %d", len(msgs))
	}
}

func TestScheduledCalendarNext(t *testing.T) {
	task := &ScheduledTask{Kind: "daily", TimeOfDay: "09:30", Timezone: "Asia/Kolkata"}
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	next := nextCalendar(task, now)
	if next == 0 {
		t.Fatal("no next daily run")
	}
	if got := time.UnixMilli(next).In(timeLocation(t, "Asia/Kolkata")).Format("15:04"); got != "09:30" {
		t.Fatalf("next time = %s", got)
	}
	if time.UnixMilli(next).Before(now) {
		t.Fatal("next run is in the past")
	}
}

func timeLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestAgentFallbackToConfiguredModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	cfg := testConfig(t, "mock")
	cfg.Providers = []Provider{
		{ID: "bad", Kind: "openai", BaseURL: upstream.URL, APIKey: "k", Models: []Model{{ID: "bad-1", Name: "Bad"}}},
		{ID: "mock", Kind: "mock", Models: []Model{{ID: "mock-1", Name: "Mock"}}},
	}
	cfg.DefaultModel = "bad-1"
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	frames := runSSE(t, ts, RunRequest{
		SessionID: "sess-fallback", RunID: "run-fallback", Model: "bad-1",
		Fallbacks: []string{"mock-1"}, FallbackStrategy: "always",
		Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "hello"}}}},
		MaxTurns: 2,
	})
	var sawInfo, sawDone bool
	for _, f := range frames {
		if f.Event == "info" {
			sawInfo = true
		}
		if f.Event == "done" {
			sawDone = true
		}
		if f.Event == "error" {
			t.Fatalf("fallback failed: %v", f.Data)
		}
	}
	if !sawInfo || !sawDone {
		t.Fatalf("fallback events info=%v done=%v", sawInfo, sawDone)
	}
}
