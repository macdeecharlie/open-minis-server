package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

func mockPrompt() *PromptContext {
	off := false
	return &PromptContext{MemoryEnabled: &off, Headless: false}
}

func mockTool(name string) ToolDef {
	return ToolDef{Type: "function", Function: ToolFunction{
		Name: name, Parameters: json.RawMessage(`{"type":"object","properties":{"tool_title":{"type":"string"}}}`),
	}}
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
		Tools:     []ToolDef{mockTool("memory_get")},
		Prompt:    mockPrompt(),
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
	out := s.buildTools(defs, false)
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
	if _, ok := byName["web_fetch"]; ok {
		t.Fatalf("interactive run gained a non-official web tool: %+v", byName)
	}
	headless := s.buildTools(defs, true)
	hasWeb := false
	for _, d := range headless {
		if d.Function.Name == "web_fetch" {
			hasWeb = true
		}
	}
	if !hasWeb {
		t.Fatal("headless run should have web_fetch")
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
		{"shell_execute", `{"command":"cat /var/minis/attachments/a"}`, true},
		{"shell_execute", `{"command":"cat /var/minis/workspace/a"}`, false},
		{"file_read", `{"path":"/var/minis/offloads/tool_1.txt"}`, false},
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
	if err := sb.RemoveSession(); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile("/var/minis/workspace/a.txt", 1, 0, 1000); err == nil {
		t.Fatal("workspace file still exists after RemoveSession")
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
		terminals: newTerminalManager(false, t.TempDir()),
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

func TestGeminiProviderParsesTextAndTools(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/models/gem-1:streamGenerateContent") {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != "k" {
			t.Errorf("api key header = %q", r.Header.Get("x-goog-api-key"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"candidates":[{"content":{"parts":[{"text":"Gemini hi"}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"candidates":[{"content":{"parts":[{"thoughtSignature":"sig-123","functionCall":{"name":"shell_execute","args":{"command":"echo hi"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3}}`+"\n\n")
	}))
	defer ts.Close()
	p := &geminiProvider{cfg: &Provider{ID: "g", Kind: "gemini", BaseURL: ts.URL, APIKey: "k"}, http: newProviderHTTP()}
	var text strings.Builder
	var call ProviderEvent
	res, err := p.Stream(context.Background(), ProviderRequest{
		Model: &Model{ID: "gem-1"}, Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "hi"}}}},
	}, func(ev ProviderEvent) error {
		switch ev.Kind {
		case "text":
			text.WriteString(ev.Text)
		case "tool_call":
			call = ev
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "Gemini hi" || call.Name != "shell_execute" || !strings.Contains(call.Args, "echo hi") || call.Signature != "sig-123" {
		t.Fatalf("text=%q call=%+v", text.String(), call)
	}
	contents, err := geminiContents([]WireMessage{{Role: "assistant", Parts: []WirePart{{
		Type: "tool_use", ID: "x", Name: "shell_execute", Args: json.RawMessage(`{"command":"echo hi"}`), ThoughtSignature: "sig-123",
	}}}})
	if err != nil || !strings.Contains(fmt.Sprint(contents), "sig-123") {
		t.Fatalf("contents did not preserve the thought signature: %+v err=%v", contents, err)
	}
	if res.StopReason != "tool_use" || res.Usage.InputTokens != 9 {
		t.Fatalf("result=%+v", res)
	}
}

func TestServerPromptAssembly(t *testing.T) {
	on := true
	prompt := buildSystemPrompt(&PromptContext{
		Identity:          "You are Test, a capable AI assistant running on an Android device with a fully functional Linux sandbox (Alpine Linux via PRoot, aarch64). ",
		MemoryEnabled:     &on,
		BrowserEnabled:    true,
		UserAskEnabled:    true,
		DelegationEnabled: true,
		Skills:            "Skills:\n<available_skills></available_skills>",
		GlobalMemory:      "Global memory (GLOBAL.md):\nhello",
		DailyMemory:       "Recent memories:\nworld",
		Language:          "en-US",
		Timezone:          "Asia/Kolkata",
		ModelUseCount:     3,
	}, "/workspace/sessions/s1/workspace")
	for _, want := range []string{
		"You are Test, a capable AI assistant running in a dedicated full Debian Linux container",
		"Available tools:",
		"- browser_use:",
		"- user_ask:",
		"Memory system (currently ENABLED):",
		"Skills:\n<available_skills>",
		"Global memory (GLOBAL.md):",
		"Recent memories:",
		"Runtime context:",
		"- Current date: ",
		"- Device language: en-US",
		"- minis-model-use models available: 3",
		"- Execution environment: dedicated Debian 12 container",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	for _, placeholder := range []string{"{{BROWSER_BULLET}}", "{{USER_ASK_LINE}}", "{{MINIS_URL_NOTE}}", "{{MEMORY_SYSTEM}}", "{{MODEL_USE_NOTE}}", "{{SCHEDULED_NOTE}}", "${toolListMemoryBullets}"} {
		if strings.Contains(prompt, placeholder) {
			t.Fatalf("prompt still contains %s", placeholder)
		}
	}
	if strings.Contains(prompt, "PRoot, aarch64") {
		t.Fatal("prompt still describes the old PRoot sandbox")
	}
	off := false
	prompt = buildSystemPrompt(&PromptContext{MemoryEnabled: &off}, "/w")
	if !strings.Contains(prompt, "Memory system (currently DISABLED)") {
		t.Fatal("memory-off prompt missing disabled notice")
	}
	if strings.Contains(prompt, "memory_write:") {
		t.Fatal("memory-off prompt advertises memory tools")
	}
}

func TestDeviceCLIRouting(t *testing.T) {
	cases := map[string]bool{
		"android-calendar list --max 5":                          true,
		"cd /var/minis/workspace && android-photos stats":        true,
		"echo hi; android-device battery":                        true,
		"/usr/local/bin/android-open https://example.com":        true,
		"ANDROID_X=1 sudo timeout 10 android-location current":   true,
		"minis-mcp-cli list":                                     false,
		"minis-config get defaults":                              true,
		"minis-model-use list":                                   true,
		"minis-scheduled list":                                   true,
		"minis-sessions-cli search --keywords budget":            true,
		"echo 'android-calendar is a CLI'":                       false,
		"python3 script.py --name android-calendar":              false,
		"rg -n 'android-clipboard' /var/minis/workspace":         false,
		"cat /var/minis/workspace/report.md | rg android-device": false,
	}
	for command, want := range cases {
		if got := usesDeviceCLI(command); got != want {
			t.Fatalf("usesDeviceCLI(%q) = %v, want %v", command, got, want)
		}
	}
	if !clientOwnedCall("shell_execute", json.RawMessage(`{"command":"android-contacts list"}`)) {
		t.Fatal("device CLI shell call was not routed to the client")
	}
	if clientOwnedCall("shell_execute", json.RawMessage(`{"command":"uname -a"}`)) {
		t.Fatal("server shell call was routed to the client")
	}
	for _, path := range []string{"/var/minis/memory/GLOBAL.md", "/var/minis/shared/a.txt", "/var/minis/mounts/x/y"} {
		if !clientOwnedCall("file_read", json.RawMessage(`{"path":"`+path+`"}`)) {
			t.Fatalf("device path %s was not routed to the client", path)
		}
	}
	if clientOwnedCall("file_read", json.RawMessage(`{"path":"/var/minis/mcp-servers/servers.json"}`)) {
		t.Fatal("MCP config should be server-owned")
	}
}

type stubProvider struct {
	reply string
	calls int
}

func (s *stubProvider) Stream(ctx context.Context, req ProviderRequest, emit func(ProviderEvent) error) (ProviderResult, error) {
	s.calls++
	if s.reply != "" {
		if err := emit(ProviderEvent{Kind: "text", Text: s.reply}); err != nil {
			return ProviderResult{}, err
		}
	}
	return ProviderResult{StopReason: "end_turn"}, nil
}

func TestContextEstimatorWeights(t *testing.T) {
	ascii := estimateTextTokens(strings.Repeat("hello world ", 100))
	cjk := estimateTextTokens(strings.Repeat("你好世界", 300))
	if ascii <= 0 || cjk <= 0 {
		t.Fatalf("estimates ascii=%d cjk=%d", ascii, cjk)
	}
	// 4 CJK characters are roughly one token each, far denser than English.
	if cjk < 1000 {
		t.Fatalf("CJK estimate too small: %d", cjk)
	}
	if estimateTextTokens("") != 0 {
		t.Fatal("empty text should be zero tokens")
	}
}

func TestCompactionKeepsToolPairing(t *testing.T) {
	messages := []WireMessage{
		{Role: "user", Parts: []WirePart{{Type: "text", Text: "old user turn"}}},
		{Role: "assistant", Parts: []WirePart{
			{Type: "text", Text: "running"},
			{Type: "tool_use", ID: "call_1", Name: "shell_execute", Args: json.RawMessage(`{"command":"ls"}`)},
		}},
		{Role: "user", Parts: []WirePart{{Type: "tool_result", ID: "call_1", Name: "shell_execute", Content: "a\nb"}}},
		{Role: "assistant", Parts: []WirePart{{Type: "text", Text: "done"}}},
		{Role: "user", Parts: []WirePart{{Type: "text", Text: "keep me"}}},
		{Role: "assistant", Parts: []WirePart{{Type: "text", Text: "recent"}}},
		{Role: "user", Parts: []WirePart{{Type: "text", Text: "latest"}}},
	}
	split := splitForCompaction(messages, 1)
	if split != 6 {
		t.Fatalf("split = %d, want 6 (keep latest user turn)", split)
	}
	out := applyCompaction(messages, split, "earlier work")
	if len(out) != 1 || out[0].Role != "user" || len(out[0].Parts) != 2 {
		t.Fatalf("compacted = %+v", out)
	}
	if !strings.Contains(out[0].Parts[0].Text, "<context-summary>") || !strings.Contains(out[0].Parts[1].Text, "latest") {
		t.Fatalf("compacted parts = %+v", out[0].Parts)
	}
}

func TestMaybeCompactSummarizesOldTurns(t *testing.T) {
	cfg := testConfig(t, "mock")
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	big := strings.Repeat("detail ", 3000)
	var messages []WireMessage
	for i := 0; i < 24; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, WireMessage{Role: role, Parts: []WirePart{{Type: "text", Text: big + fmt.Sprintf(" turn=%d", i)}}})
	}
	run := newRun(context.Background(), "run_compact")
	defer run.close()
	stub := &stubProvider{reply: "The user investigated a large dataset."}
	compactions := 0
	out, changed, err := s.maybeCompact(run, messages, "system", nil, 64_000, stub, &Model{ID: "mock-1"}, 0, &compactions)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || compactions != 1 {
		t.Fatalf("changed=%v compactions=%d", changed, compactions)
	}
	if len(out) >= len(messages) {
		t.Fatalf("compaction did not shrink history: %d -> %d", len(messages), len(out))
	}
	found := false
	for _, m := range out {
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "<context-summary>") && strings.Contains(p.Text, "large dataset") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("compacted history does not carry the summary")
	}
	if stub.calls != 1 {
		t.Fatalf("summary calls = %d", stub.calls)
	}
}

func TestToolResultOffload(t *testing.T) {
	root := t.TempDir()
	sb, err := NewSandbox(SandboxConfig{WorkRoot: root, MaxOutput: 1_000_000}, "sess-offload")
	if err != nil {
		t.Fatal(err)
	}
	res := ToolResult{ID: "call_1", Name: "shell_execute", Content: strings.Repeat("x", 55*1024)}
	offloadToolResult(sb, &res, true)
	if !strings.Contains(res.Content, "/var/minis/offloads/tools/call_1.txt") {
		t.Fatalf("stub = %q", res.Content)
	}
	content, err := sb.ReadFile("/var/minis/offloads/tools/call_1.txt", 1, 0, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) < 50*1024 {
		t.Fatalf("offloaded content = %d bytes", len(content))
	}
}

func TestCLIEndpoints(t *testing.T) {
	cfg := testConfig(t, "mock")
	cfg.Sandbox.AgentToken = "sandbox-token"
	store, err := NewStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	call := func(method, path string, body any, out any) error {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rdr)
		req.Header.Set("Authorization", "Bearer sandbox-token")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil
	}

	// Application token must not work on the CLI surface.
	bad, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/cli/models", nil)
	bad.Header.Set("Authorization", "Bearer test-token")
	badResp, err := http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("app token on CLI route = HTTP %d, want 401", badResp.StatusCode)
	}

	// A stored session is visible through sessions list/messages/update.
	if _, err := store.AppendMessage(StoredMessage{
		ID: "m1", SessionID: "sess-cli", Role: "user",
		Parts: []WirePart{{Type: "text", Text: "budget report for October"}}, CreatedAt: nowMillis(),
	}); err != nil {
		t.Fatal(err)
	}
	var sessions struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := call("GET", "/v1/cli/sessions?q=budget", nil, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions.Sessions) != 1 || sessions.Sessions[0]["session_id"] != "sess-cli" {
		t.Fatalf("sessions = %+v", sessions.Sessions)
	}
	var messages struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := call("GET", "/v1/cli/sessions/sess-cli/messages?tools=1", nil, &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages.Messages) != 1 || !strings.Contains(fmt.Sprint(messages.Messages[0]), "October") {
		t.Fatalf("messages = %+v", messages.Messages)
	}
	if err := call("POST", "/v1/cli/sessions/sess-cli/update", map[string]any{"title": "Budget"}, nil); err != nil {
		t.Fatal(err)
	}
	meta, err := store.GetSession("sess-cli")
	if err != nil || meta.Title != "Budget" {
		t.Fatalf("session title = %+v err=%v", meta, err)
	}

	// Model use goes through the configured provider.
	var used struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	if err := call("POST", "/v1/cli/model-use", map[string]any{
		"model": "mock-1", "messages": []map[string]any{{"role": "user", "content": "hello"}},
	}, &used); err != nil {
		t.Fatal(err)
	}
	if used.Model != "mock-1" || !strings.Contains(used.Text, "OpenMinis server is connected") {
		t.Fatalf("model use = %+v", used)
	}

	// Scheduled create/list through the CLI surface.
	var task map[string]any
	if err := call("POST", "/v1/cli/scheduled", map[string]any{
		"label": "cli task", "prompt": "run [[shell:echo cli-ok]]", "target": "new",
		"enabled": true, "kind": "interval", "interval_sec": 60,
	}, &task); err != nil {
		t.Fatal(err)
	}
	if task["id"] == nil || task["id"] == "" {
		t.Fatalf("task = %+v", task)
	}
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := call("GET", "/v1/cli/scheduled", nil, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 1 {
		t.Fatalf("tasks = %d", len(list.Tasks))
	}
	if err := call("DELETE", "/v1/cli/scheduled/"+task["id"].(string), nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRequestImageBudget(t *testing.T) {
	big := strings.Repeat("A", 12*1024*1024)
	messages := []WireMessage{
		{Role: "user", Parts: []WirePart{{Type: "text", Text: "old"}, {Type: "image", Mime: "image/png", Data: big}}},
		{Role: "assistant", Parts: []WirePart{{Type: "text", Text: "ok"}}},
		{Role: "user", Parts: []WirePart{{Type: "text", Text: "new"}, {Type: "image", Mime: "image/png", Data: "small"}}},
	}
	out, dropped := applyImageBudget(messages)
	if dropped != 1 {
		t.Fatalf("dropped = %d", dropped)
	}
	if out[0].Parts[1].Type != "text" || !strings.Contains(out[0].Parts[1].Text, "omitted") {
		t.Fatalf("old image = %+v", out[0].Parts[1])
	}
	if out[2].Parts[1].Type != "image" {
		t.Fatalf("new image was dropped: %+v", out[2].Parts[1])
	}
}

func TestSessionMetaAndDeleteRemovesWorkspace(t *testing.T) {
	cfg := testConfig(t, "mock")
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	do := func(method, path string, body any) int {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rdr)
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if _, err := store.EnsureSession("sess-meta", "mock-1"); err != nil {
		t.Fatal(err)
	}
	if code := do("POST", "/v1/sessions/sess-meta/meta", map[string]any{"title": "Renamed"}); code != 200 {
		t.Fatalf("meta HTTP %d", code)
	}
	meta, err := store.GetSession("sess-meta")
	if err != nil || meta.Title != "Renamed" {
		t.Fatalf("meta = %+v err=%v", meta, err)
	}
	if _, err := store.AppendMessage(StoredMessage{ID: "u1", SessionID: "sess-meta", Role: "user", Parts: []WirePart{{Type: "text", Text: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	sb, _ := NewSandbox(cfg.Sandbox, "sess-meta")
	if err := sb.WriteFile("/var/minis/workspace/gone.txt", "x", false, true); err != nil {
		t.Fatal(err)
	}
	if code := do("DELETE", "/v1/sessions/sess-meta", nil); code != 204 {
		t.Fatalf("delete HTTP %d", code)
	}
	if _, err := store.GetSession("sess-meta"); err == nil {
		t.Fatal("session meta still exists")
	}
	if _, err := sb.ReadFile("/var/minis/workspace/gone.txt", 1, 0, 100); err == nil {
		t.Fatal("session workspace still exists")
	}
}

func TestStoredCompactionIsReused(t *testing.T) {
	messages := []WireMessage{
		{ID: "m1", Role: "user", Parts: []WirePart{{Type: "text", Text: "old"}}},
		{ID: "m2", Role: "assistant", Parts: []WirePart{{Type: "text", Text: "old reply"}}},
		{ID: "m3", Role: "user", Parts: []WirePart{{Type: "text", Text: "recent"}}},
	}
	out, ok := applyStoredCompaction(messages, "summary of old", "m2")
	if !ok || len(out) != 1 {
		t.Fatalf("applied=%v out=%+v", ok, out)
	}
	if !strings.Contains(out[0].Parts[0].Text, "summary of old") || !strings.Contains(out[0].Parts[1].Text, "recent") {
		t.Fatalf("out = %+v", out[0].Parts)
	}
	if _, ok := applyStoredCompaction(messages, "summary", "missing"); ok {
		t.Fatal("applied a summary whose anchor is absent")
	}
}

func TestSandboxFileOperations(t *testing.T) {
	sb, err := NewSandbox(SandboxConfig{WorkRoot: t.TempDir(), MaxOutput: 1_000_000}, "sess-ops")
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Mkdir("/var/minis/workspace/a/b"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("/var/minis/workspace/a/b/x.txt", "one", false, true); err != nil {
		t.Fatal(err)
	}
	if err := sb.Rename("/var/minis/workspace/a/b/x.txt", "/var/minis/workspace/a/y.txt"); err != nil {
		t.Fatal(err)
	}
	if got, err := sb.ReadFile("/var/minis/workspace/a/y.txt", 1, 0, 100); err != nil || !strings.Contains(got, "one") {
		t.Fatalf("renamed read = %q err=%v", got, err)
	}
	if err := sb.Delete("/var/minis/workspace/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ListDir("/var/minis/workspace/a"); err == nil {
		t.Fatal("deleted directory still lists")
	}
	if err := sb.Delete("/var/minis/workspace"); err == nil {
		t.Fatal("workspace root was deleted")
	}
}

func TestRemoteSandboxFileOperations(t *testing.T) {
	agent := &sandboxAgent{token: "t", workRoot: t.TempDir(), maxOutput: 1_000_000, terminals: newTerminalManager(false, t.TempDir())}
	ts := httptest.NewServer(agent.handler())
	defer ts.Close()
	sb, err := NewRemoteSandbox(SandboxConfig{Mode: "agent", AgentURL: ts.URL, AgentToken: "t"}, "sess-remote-ops")
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Mkdir("/var/minis/workspace/d"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("/var/minis/workspace/d/a.txt", "a", false, true); err != nil {
		t.Fatal(err)
	}
	if err := sb.Rename("/var/minis/workspace/d/a.txt", "/var/minis/workspace/d/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile("/var/minis/workspace/d/b.txt", 1, 0, 100); err != nil {
		t.Fatal(err)
	}
	if err := sb.Delete("/var/minis/workspace/d"); err != nil {
		t.Fatal(err)
	}
}

func TestAuxiliaryOpenAIOnAnthropicProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}\n\n")
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Aux title\"}}\n\n")
		io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	cfg := testConfig(t, "mock")
	cfg.Providers = []Provider{{
		ID: "claude", Kind: "anthropic", BaseURL: upstream.URL, APIKey: "k",
		Models: []Model{{ID: "claude-1", Name: "Claude", ContextWindow: 200000, MaxOutputTokens: 8192}},
	}}
	cfg.DefaultModel = "claude-1"
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"model":"claude-1","stream":true,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"title this"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if !strings.Contains(text, "Aux title") || !strings.Contains(text, "[DONE]") || !strings.Contains(text, "chat.completion.chunk") {
		t.Fatalf("translated stream = %s", text)
	}

	body = `{"model":"claude-1","messages":[{"role":"user","content":"title this"}]}`
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(out), "Aux title") {
		t.Fatalf("translated completion = %+v", out)
	}
}

func TestRunEventReplay(t *testing.T) {
	cfg := testConfig(t, "mock")
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req := RunRequest{
		SessionID: "sess-replay", RunID: "run-replay", Model: "mock-1",
		Tools:    []ToolDef{mockTool("memory_get")},
		Prompt:   mockPrompt(),
		Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "please [[client:memory_get]]"}}}},
	}
	body, _ := json.Marshal(req)
	hreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs", bytes.NewReader(body))
	hreq.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var readyID int
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: await_client") {
			break
		}
		if strings.HasPrefix(line, "id: ") {
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), "%d", &readyID)
		}
	}
	resp.Body.Close()
	if readyID == 0 {
		t.Fatal("no event id was sent")
	}
	// The run is still waiting for the client tool; reconnect from the middle.
	ereq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/agent/runs/run-replay/events?after=2", nil)
	ereq.Header.Set("Authorization", "Bearer test-token")
	eresp, err := http.DefaultClient.Do(ereq)
	if err != nil {
		t.Fatal(err)
	}
	defer eresp.Body.Close()
	esc := bufio.NewScanner(eresp.Body)
	esc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	sawAwait := false
	lastID := 0
	for esc.Scan() {
		line := esc.Text()
		if strings.HasPrefix(line, "id: ") {
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), "%d", &lastID)
		}
		if strings.HasPrefix(line, "event: await_client") {
			sawAwait = true
			break
		}
	}
	if !sawAwait || lastID <= 2 {
		t.Fatalf("replay did not resume after id 2: await=%v last=%d", sawAwait, lastID)
	}
	// Cancel so the blocked run does not linger.
	creq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs/run-replay/cancel", nil)
	creq.Header.Set("Authorization", "Bearer test-token")
	cresp, err := http.DefaultClient.Do(creq)
	if err != nil {
		t.Fatal(err)
	}
	cresp.Body.Close()
	if cresp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel HTTP %d", cresp.StatusCode)
	}
}

func TestRunSteering(t *testing.T) {
	cfg := testConfig(t, "mock")
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req := RunRequest{
		SessionID: "sess-steer", RunID: "run-steer", Model: "mock-1",
		Tools:    []ToolDef{mockTool("memory_get")},
		Prompt:   mockPrompt(),
		Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "please [[client:memory_get]]"}}}},
	}
	body, _ := json.Marshal(req)
	hreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs", bytes.NewReader(body))
	hreq.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var toolID string
	sawSteered := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			var m map[string]any
			_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &m)
			if id, ok := m["id"].(string); ok && toolID == "" && m["name"] == "memory_get" {
				toolID = id
			}
		}
		if strings.HasPrefix(line, "event: await_client") {
			sreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs/run-steer/steer", strings.NewReader(`{"text":"also mention the moon"}`))
			sreq.Header.Set("Authorization", "Bearer test-token")
			sreq.Header.Set("Content-Type", "application/json")
			sresp, err := http.DefaultClient.Do(sreq)
			if err != nil {
				t.Fatal(err)
			}
			sresp.Body.Close()
			if sresp.StatusCode != http.StatusAccepted {
				t.Fatalf("steer HTTP %d", sresp.StatusCode)
			}
			payload, _ := json.Marshal(ToolResult{ID: toolID, Name: "memory_get", Content: "client says hi"})
			preq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs/run-steer/tool_result", bytes.NewReader(payload))
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
		if strings.HasPrefix(line, "event: steered") {
			sawSteered = true
		}
		if strings.Contains(line, "also mention the moon") {
			sawSteered = true
		}
	}
	if !sawSteered {
		t.Fatal("steer was not observed by the run")
	}
}

func TestGlobalMcpConfigIsSharedAcrossSessions(t *testing.T) {
	root := t.TempDir()
	cfg := SandboxConfig{WorkRoot: filepath.Join(root, "sessions"), GlobalRoot: filepath.Join(root, "global"), MaxOutput: 1_000_000}
	a, err := NewSandbox(cfg, "sess-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSandbox(cfg, "sess-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WriteFile("/var/minis/mcp-servers/servers.json", `{"mcpServers":{"demo":{"command":"echo"}}}`, false, true); err != nil {
		t.Fatal(err)
	}
	got, err := b.ReadFile("/var/minis/mcp-servers/servers.json", 1, 0, 1000)
	if err != nil || !strings.Contains(got, "mcpServers") {
		t.Fatalf("shared read = %q err=%v", got, err)
	}
	res, err := b.RunShell(context.Background(), "cat /var/minis/mcp-servers/servers.json", 10*time.Second, 0, nil)
	if err != nil || !strings.Contains(res.Output, "mcpServers") {
		t.Fatalf("rewritten shell read = %q err=%v", res.Output, err)
	}
	if _, err := b.ReadFile("/var/minis/workspace/mcp-servers/servers.json", 1, 0, 100); err == nil {
		t.Fatal("global MCP config leaked into the session workspace")
	}
}

func TestCLIConfigConfirmation(t *testing.T) {
	cfg := testConfig(t, "mock")
	cfg.Sandbox.AgentToken = "sandbox-token"
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	call := func(path string, body any, out any) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer sandbox-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if out != nil && resp.StatusCode < 300 {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatalf("decode %s: %v (%s)", path, err, raw)
			}
		}
		return resp.StatusCode
	}
	var pending struct {
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	if code := call("/v1/cli/config", map[string]any{"args": []string{"default_model", "mock-1"}}, &pending); code != 200 {
		t.Fatalf("set HTTP %d", code)
	}
	if pending.Status != "pending_confirmation" || pending.Token == "" {
		t.Fatalf("pending = %+v", pending)
	}
	if code := call("/v1/cli/config", map[string]any{"args": []string{"providers", "x"}}, nil); code != 400 {
		t.Fatalf("unsupported set HTTP %d, want 400", code)
	}
	var confirmed map[string]any
	if code := call("/v1/cli/config/confirm", map[string]any{"token": pending.Token}, &confirmed); code != 200 {
		t.Fatalf("confirm HTTP %d", code)
	}
	if s.cfg.DefaultModel != "mock-1" {
		t.Fatalf("default model = %q", s.cfg.DefaultModel)
	}
	if _, err := os.Stat(cfg.DataDir + "/config.json"); err != nil {
		t.Fatalf("config was not persisted: %v", err)
	}
	// A token is one-shot.
	if code := call("/v1/cli/config/confirm", map[string]any{"token": pending.Token}, nil); code != 404 {
		t.Fatalf("reused token HTTP %d, want 404", code)
	}
}

func TestCompletedRunReplay(t *testing.T) {
	cfg := testConfig(t, "mock")
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req := RunRequest{
		SessionID: "sess-done-replay", RunID: "run-done-replay", Model: "mock-1",
		Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "run [[shell:echo finished-replay]]"}}}},
	}
	body, _ := json.Marshal(req)
	hreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs", bytes.NewReader(body))
	hreq.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	time.Sleep(50 * time.Millisecond)
	ereq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/agent/runs/run-done-replay/events?after=0", nil)
	ereq.Header.Set("Authorization", "Bearer test-token")
	eresp, err := http.DefaultClient.Do(ereq)
	if err != nil {
		t.Fatal(err)
	}
	defer eresp.Body.Close()
	raw, _ := io.ReadAll(eresp.Body)
	if !strings.Contains(string(raw), "finished-replay") || !strings.Contains(string(raw), "event: done") {
		t.Fatalf("completed replay = %s", raw)
	}
}

func TestOpenAIRequestParameters(t *testing.T) {
	var body map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer upstream.Close()
	p := &openAIProvider{cfg: &Provider{ID: "o", Kind: "openai", BaseURL: upstream.URL, APIKey: "k"}, http: newProviderHTTP()}
	_, err := p.Stream(context.Background(), ProviderRequest{
		Model:     &Model{ID: "gpt-5.6-sol", SupportsReasoning: true},
		Messages:  []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "hi"}}}},
		Thinking:  "ultra",
		MaxTokens: 100,
	}, func(ProviderEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body["max_completion_tokens"]; !ok {
		t.Fatalf("gpt-5 request did not use max_completion_tokens: %+v", body)
	}
	if body["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %v", body["reasoning_effort"])
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatalf("request sent both token fields: %+v", body)
	}
}

func TestProviderStreamIdleTimeout(t *testing.T) {
	old := providerIdleTimeout
	providerIdleTimeout = 200 * time.Millisecond
	defer func() { providerIdleTimeout = old }()

	release := make(chan struct{})
	defer close(release)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()

	p := &openAIProvider{cfg: &Provider{ID: "o", Kind: "openai", BaseURL: upstream.URL, APIKey: "k"}, http: newProviderHTTP()}
	start := time.Now()
	_, err := p.Stream(context.Background(), ProviderRequest{
		Model: &Model{ID: "m"}, Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "hi"}}}},
	}, func(ProviderEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "idle") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("idle watchdog took too long: %s", time.Since(start))
	}
}

func TestCancelStopsRun(t *testing.T) {
	cfg := testConfig(t, "mock")
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req := RunRequest{
		SessionID: "sess-cancel", RunID: "run-cancel", Model: "mock-1",
		Tools:    []ToolDef{mockTool("memory_get")},
		Prompt:   mockPrompt(),
		Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "please [[client:memory_get]]"}}}},
	}
	body, _ := json.Marshal(req)
	hreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs", bytes.NewReader(body))
	hreq.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	toolID := ""
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: await_client") {
			break
		}
		if strings.Contains(sc.Text(), "call_mock_client") && toolID == "" {
			toolID = "call_mock_client"
		}
	}
	creq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs/run-cancel/cancel", nil)
	creq.Header.Set("Authorization", "Bearer test-token")
	cresp, err := http.DefaultClient.Do(creq)
	if err != nil {
		t.Fatal(err)
	}
	cresp.Body.Close()
	if cresp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel HTTP %d", cresp.StatusCode)
	}
	resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	payload, _ := json.Marshal(ToolResult{ID: toolID, Name: "memory_get", Content: "late"})
	preq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs/run-cancel/tool_result", bytes.NewReader(payload))
	preq.Header.Set("Authorization", "Bearer test-token")
	pres, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatal(err)
	}
	pres.Body.Close()
	if pres.StatusCode != http.StatusNotFound {
		t.Fatalf("late tool result HTTP %d, want 404 after cancel", pres.StatusCode)
	}
}

func TestRepairToolPairing(t *testing.T) {
	messages := []WireMessage{
		{Role: "user", Parts: []WirePart{{Type: "text", Text: "go"}}},
		{Role: "assistant", Parts: []WirePart{{Type: "tool_use", ID: "a", Name: "shell_execute", Args: json.RawMessage(`{"command":"ls"}`)}}},
		{Role: "user", Parts: []WirePart{{Type: "text", Text: "interrupted"}}},
		{Role: "assistant", Parts: []WirePart{{Type: "text", Text: "done"}}},
	}
	out := repairToolPairing(messages)
	if len(out) != 4 {
		t.Fatalf("out = %+v", out)
	}
	found := false
	for _, m := range out {
		for _, p := range m.Parts {
			if p.Type == "tool_result" && p.ID == "a" && p.IsError {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("missing tool result was not repaired")
	}
	orphan := []WireMessage{
		{Role: "user", Parts: []WirePart{{Type: "tool_result", ID: "gone", Name: "x", Content: "stale"}, {Type: "text", Text: "hi"}}},
	}
	out = repairToolPairing(orphan)
	if len(out) != 1 || len(out[0].Parts) != 1 || out[0].Parts[0].Type != "text" {
		t.Fatalf("orphan result was not dropped: %+v", out)
	}
}

func TestAudioInputMapping(t *testing.T) {
	msgs, err := openAIMessages([]WireMessage{{Role: "user", Parts: []WirePart{
		{Type: "text", Text: "transcribe"},
		{Type: "audio", Format: "wav", Mime: "audio/wav", Data: "YWJj"},
	}}}, false, false)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("msgs=%+v err=%v", msgs, err)
	}
	blob := fmt.Sprint(msgs[0]["content"])
	if !strings.Contains(blob, "input_audio") || !strings.Contains(blob, "wav") {
		t.Fatalf("audio content = %s", blob)
	}
	contents, err := geminiContents([]WireMessage{{Role: "user", Parts: []WirePart{
		{Type: "audio", Format: "mp3", Mime: "audio/mp3", Data: "YWJj"},
	}}})
	if err != nil || !strings.Contains(fmt.Sprint(contents), "audio/mp3") {
		t.Fatalf("gemini audio = %v err=%v", contents, err)
	}
	if _, err := anthropicMessages([]WireMessage{{Role: "user", Parts: []WirePart{
		{Type: "audio", Format: "wav", Data: "YWJj"},
	}}}, false); err == nil {
		t.Fatal("Anthropic audio should fail visibly")
	}
}

func TestMinisURL(t *testing.T) {
	got := minisURL("/var/minis/workspace/报告 1.md")
	want := "minis://workspace/%E6%8A%A5%E5%91%8A%201.md"
	if got != want {
		t.Fatalf("minisURL = %q, want %q", got, want)
	}
	if minisURL("/tmp/x") != "" {
		t.Fatal("non-virtual path produced a minis URL")
	}
}

func TestCLIBinariesStart(t *testing.T) {
	for _, name := range []string{"minis-sessions-cli", "minis-config", "minis-model-use", "minis-scheduled"} {
		if code := runCLI(name, []string{"--help"}); code != 0 {
			t.Fatalf("%s --help exit %d", name, code)
		}
	}
}

func TestToolResultImageMapping(t *testing.T) {
	msg := WireMessage{Role: "user", Parts: []WirePart{{
		Type: "tool_result", ID: "call_1", Name: "browser_use", Content: "screenshot",
		Mime: "image/png", Data: "aW1n",
	}}}
	msgs, err := openAIMessages([]WireMessage{msg}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	blob := fmt.Sprint(msgs)
	if !strings.Contains(blob, "image_url") || !strings.Contains(blob, "data:image/png;base64,aW1n") || !strings.Contains(blob, "tool_call_id: call_1") {
		t.Fatalf("OpenAI carrier missing: %s", blob)
	}
	if _, err := openAIMessages([]WireMessage{msg}, false, false); err != nil {
		t.Fatal(err)
	}
	amsgs, err := anthropicMessages([]WireMessage{msg}, false)
	if err != nil || !strings.Contains(fmt.Sprint(amsgs), "image") {
		t.Fatalf("Anthropic tool image missing: %v err=%v", amsgs, err)
	}
	gmsgs, err := geminiContents([]WireMessage{msg})
	if err != nil || !strings.Contains(fmt.Sprint(gmsgs), "inlineData") {
		t.Fatalf("Gemini tool image missing: %v err=%v", gmsgs, err)
	}
}

func TestImageBudgetToolResult(t *testing.T) {
	out, dropped := applyImageBudget([]WireMessage{{Role: "user", Parts: []WirePart{{
		Type: "tool_result", ID: "c", Name: "browser_use", Content: "shot", Mime: "image/png", Data: strings.Repeat("A", 11*1024*1024),
	}}}})
	if dropped != 1 {
		t.Fatalf("dropped = %d", dropped)
	}
	if out[0].Parts[0].Data != "" || !strings.Contains(out[0].Parts[0].Content, "image omitted") {
		t.Fatalf("tool image was not omitted cleanly: %+v", out[0].Parts[0])
	}
}

func TestClientToolImageResult(t *testing.T) {
	cfg := testConfig(t, "mock")
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req := RunRequest{SessionID: "sess-imgtool", RunID: "run-imgtool", Model: "mock-1",
		Tools:    []ToolDef{mockTool("read_image")},
		Prompt:   mockPrompt(),
		Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "look [[client:read_image]]"}}}}}
	body, _ := json.Marshal(req)
	hreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs", bytes.NewReader(body))
	hreq.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var toolID string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			var m map[string]any
			_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &m)
			if m["name"] == "read_image" {
				if id, ok := m["id"].(string); ok {
					toolID = id
				}
			}
		}
		if strings.HasPrefix(line, "event: await_client") {
			payload, _ := json.Marshal(ToolResult{
				ID: toolID, Name: "read_image", Content: "image", Image: "aW1n", ImageMime: "image/png", ImagePath: "/var/minis/attachments/x.png",
			})
			preq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/runs/run-imgtool/tool_result", bytes.NewReader(payload))
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
		if strings.HasPrefix(line, "event: done") {
			break
		}
	}
	msgs, err := store.Messages("sess-imgtool")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == "tool_result" && p.Data == "aW1n" && p.Mime == "image/png" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("image tool result was not persisted: %+v", msgs)
	}
}

func TestTerminalReattach(t *testing.T) {
	agent := &sandboxAgent{
		token: "t", workRoot: t.TempDir(), maxOutput: 10000,
		terminals: newTerminalManager(false, t.TempDir()),
	}
	ts := httptest.NewServer(agent.handler())
	defer ts.Close()

	body := strings.NewReader(`{"session_id":"sess-reattach","cols":100,"rows":30}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/terminal", body)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	termID := ""
	received := int64(0)
	sent := false
	readUntil := func(want string) {
		deadline := time.Now().Add(10 * time.Second)
		for sc.Scan() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %q", want)
			}
			line := sc.Text()
			if strings.HasPrefix(line, "event:") {
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var d map[string]any
			_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &d)
			if id, ok := d["id"].(string); ok && termID == "" {
				termID = id
			}
			if enc, ok := d["data"].(string); ok {
				raw, _ := base64.StdEncoding.DecodeString(enc)
				off, _ := d["offset"].(float64)
				received = int64(off) + int64(len(raw))
				if strings.Contains(string(raw), want) {
					return
				}
			}
		}
		t.Fatalf("stream ended waiting for %q", want)
	}
	send := func(text string) {
		ireq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/terminal/"+termID+"/input", strings.NewReader(text))
		ireq.Header.Set("Authorization", "Bearer t")
		iresp, err := http.DefaultClient.Do(ireq)
		if err != nil {
			t.Fatal(err)
		}
		iresp.Body.Close()
	}
	// First event is ready; scanner helper skips it, so send after a short
	// read of the ready frame.
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: ready") {
			// data line follows
			sc.Scan()
			var d map[string]any
			_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(sc.Text(), "data:"))), &d)
			termID, _ = d["id"].(string)
			break
		}
	}
	if termID == "" {
		t.Fatal("no terminal id")
	}
	send("echo first-ok\r")
	sent = true
	_ = sent
	readUntil("first-ok")
	resp.Body.Close()
	time.Sleep(100 * time.Millisecond)

	areq, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v1/terminal/%s?after=%d", ts.URL, termID, received), nil)
	areq.Header.Set("Authorization", "Bearer t")
	aresp, err := http.DefaultClient.Do(areq)
	if err != nil {
		t.Fatal(err)
	}
	defer aresp.Body.Close()
	asc := bufio.NewScanner(aresp.Body)
	asc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for asc.Scan() {
		if strings.HasPrefix(asc.Text(), "event: ready") {
			break
		}
	}
	// New input while attached.
	ireq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/terminal/"+termID+"/input", strings.NewReader("echo second-ok\r"))
	ireq.Header.Set("Authorization", "Bearer t")
	iresp, err := http.DefaultClient.Do(ireq)
	if err != nil {
		t.Fatal(err)
	}
	iresp.Body.Close()
	deadline := time.Now().Add(10 * time.Second)
	ok := false
	for asc.Scan() {
		if time.Now().After(deadline) {
			break
		}
		line := asc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var d map[string]any
		_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &d)
		if enc, okEnc := d["data"].(string); okEnc {
			raw, _ := base64.StdEncoding.DecodeString(enc)
			if strings.Contains(string(raw), "second-ok") {
				ok = true
				break
			}
		}
	}
	if !ok {
		t.Fatal("reattached terminal did not receive input/output")
	}
	creq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/terminal/"+termID+"/close", nil)
	creq.Header.Set("Authorization", "Bearer t")
	cresp, err := http.DefaultClient.Do(creq)
	if err != nil {
		t.Fatal(err)
	}
	cresp.Body.Close()
}

func TestResponsesProviderStreamAndInput(t *testing.T) {
	var body map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/responses") {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello \"}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"shell_execute\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{\\\"command\\\":\\\"echo hi\\\"}\"}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"shell_execute\",\"arguments\":\"{\\\"command\\\":\\\"echo hi\\\"}\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := &responsesProvider{cfg: &Provider{ID: "oa", Kind: "openai-responses", BaseURL: upstream.URL, APIKey: "k"}, http: newProviderHTTP()}
	var text strings.Builder
	var call ProviderEvent
	res, err := p.Stream(context.Background(), ProviderRequest{
		Model:  &Model{ID: "gpt-5.6", SupportsReasoning: true, InputModalities: []string{"text", "image"}},
		System: "be brief",
		Messages: []WireMessage{
			{Role: "user", Parts: []WirePart{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Parts: []WirePart{{Type: "tool_use", ID: "call_0", Name: "file_read", Args: json.RawMessage(`{"path":"/x"}`)}}},
			{Role: "user", Parts: []WirePart{{Type: "tool_result", ID: "call_0", Name: "file_read", Content: "data", Mime: "image/png", Data: "aW1n"}}},
		},
		Tools:     []ToolDef{{Type: "function", Function: ToolFunction{Name: "shell_execute", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		Thinking:  "high",
		MaxTokens: 100,
	}, func(ev ProviderEvent) error {
		if ev.Kind == "text" {
			text.WriteString(ev.Text)
		}
		if ev.Kind == "tool_call" {
			call = ev
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "Hello " || call.ID != "call_1" || call.Name != "shell_execute" || !strings.Contains(call.Args, "echo hi") {
		t.Fatalf("text=%q call=%+v", text.String(), call)
	}
	if res.StopReason != "tool_use" || res.Usage.InputTokens != 7 || res.Usage.CacheReadInputTokens != 3 || res.Usage.ReasoningTokens != 2 || res.Usage.LatestContextTokens != 10 {
		t.Fatalf("result=%+v", res)
	}
	if body["instructions"] != "be brief" || body["max_output_tokens"] != float64(100) {
		t.Fatalf("body = %+v", body)
	}
	blob := fmt.Sprint(body["input"])
	if !strings.Contains(blob, "function_call") || !strings.Contains(blob, "function_call_output") || !strings.Contains(blob, "input_image") {
		t.Fatalf("input mapping = %s", blob)
	}
	if !strings.Contains(fmt.Sprint(body["tools"]), "shell_execute") {
		t.Fatalf("tools = %v", body["tools"])
	}
}

func TestGeminiThinkingConfig(t *testing.T) {
	cfg := geminiThinkingConfig("gemini-3-pro", "xhigh")
	if cfg == nil || cfg["thinkingLevel"] != "high" || cfg["includeThoughts"] != true {
		t.Fatalf("gemini-3 config = %+v", cfg)
	}
	cfg = geminiThinkingConfig("gemini-2.5-pro", "medium")
	if cfg == nil || cfg["thinkingBudget"] != 4096 {
		t.Fatalf("2.5 pro config = %+v", cfg)
	}
	if geminiThinkingConfig("gemini-2.5-flash-lite", "high") != nil {
		t.Fatal("flash-lite should have no thinking config")
	}
	if geminiThinkingConfig("gemini-2.5-pro", "off") != nil {
		t.Fatal("off should omit thinking config")
	}
}

func TestAnthropicUnsignedThinkingEcho(t *testing.T) {
	msgs := []WireMessage{{Role: "assistant", ReasoningContent: "thought", Parts: []WirePart{{Type: "text", Text: "answer"}}}}
	with, err := anthropicMessages(msgs, true)
	if err != nil || !strings.Contains(fmt.Sprint(with), "thinking") {
		t.Fatalf("echo missing: %v err=%v", with, err)
	}
	without, err := anthropicMessages(msgs, false)
	if err != nil || strings.Contains(fmt.Sprint(without), "thought") {
		t.Fatalf("unsigned thinking leaked to native Anthropic: %v err=%v", without, err)
	}
	if anthropicEchoesUnsignedThinking("https://api.anthropic.com", &Model{SupportsReasoning: true}) {
		t.Fatal("native Anthropic must not echo unsigned thinking")
	}
	if !anthropicEchoesUnsignedThinking("https://proxy.example/v1", &Model{SupportsReasoning: true}) {
		t.Fatal("custom Anthropic proxy should echo unsigned thinking")
	}
}

func TestImageTokenEstimateIsBounded(t *testing.T) {
	big := strings.Repeat("A", 2*1024*1024)
	if n := estimateImageTokens(big); n > 4000 || n < 85 {
		t.Fatalf("image estimate = %d", n)
	}
	if n := estimateImageTokens(""); n != 0 {
		t.Fatalf("empty image estimate = %d", n)
	}
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2nXQAAAAASUVORK5CYII="
	if n := estimateImageTokens(png); n != 85 {
		t.Fatalf("1x1 PNG estimate = %d", n)
	}
}

func TestCompactionOnSingleTurn(t *testing.T) {
	cfg := testConfig(t, "mock")
	store, _ := NewStore(cfg.DataDir)
	s := NewServer(cfg, store, cfg.DataDir+"/config.json")
	run := newRun(context.Background(), "run-single-turn")
	defer run.close()
	big := strings.Repeat("tool output ", 12000) // ~144k chars
	messages := []WireMessage{
		{ID: "u1", Role: "user", Parts: []WirePart{{Type: "text", Text: "do the task"}}},
	}
	for i := 0; i < 8; i++ {
		messages = append(messages,
			WireMessage{ID: fmt.Sprintf("a%d", i), Role: "assistant", Parts: []WirePart{
				{Type: "text", Text: strings.Repeat("assistant notes ", 10000)},
				{Type: "tool_use", ID: fmt.Sprintf("c%d", i), Name: "shell_execute", Args: json.RawMessage(`{"command":"x"}`)},
			}},
			WireMessage{ID: fmt.Sprintf("t%d", i), Role: "user", Parts: []WirePart{{Type: "tool_result", ID: fmt.Sprintf("c%d", i), Name: "shell_execute", Content: big}}},
		)
	}
	stub := &stubProvider{reply: "A long single turn was compacted."}
	compactions := 0
	out, changed, err := s.maybeCompact(run, messages, "system", nil, 64_000, stub, &Model{ID: "mock-1"}, 0, &compactions)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || compactions != 1 {
		t.Fatalf("changed=%v compactions=%d out=%d", changed, compactions, len(out))
	}
	if !strings.Contains(fmt.Sprint(out), "single turn was compacted") {
		t.Fatal("summary missing from single-turn compaction")
	}
	if out[0].ID != "u1" {
		t.Fatalf("original user prompt was not kept: %+v", out[0])
	}
}

func TestResponsesIncompleteContentFilter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"content_filter\"},\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n")
	}))
	defer upstream.Close()
	p := &responsesProvider{cfg: &Provider{ID: "oa", Kind: "openai-responses", BaseURL: upstream.URL, APIKey: "k"}, http: newProviderHTTP()}
	res, err := p.Stream(context.Background(), ProviderRequest{
		Model: &Model{ID: "gpt-5.6"}, Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "x"}}}},
	}, func(ProviderEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "refusal" || res.Usage.LatestContextTokens != 20 {
		t.Fatalf("result=%+v", res)
	}
}

func TestResponsesAutoRouting(t *testing.T) {
	if !shouldUseResponsesAPI("https://api.openai.com/v1", "gpt-5.6") {
		t.Fatal("direct GPT-5 should use Responses")
	}
	if shouldUseResponsesAPI("https://openrouter.ai/api/v1", "openai/gpt-5.6") {
		t.Fatal("OpenRouter should keep Chat Completions")
	}
	if shouldUseResponsesAPI("https://api.openai.com/v1", "gpt-4o") {
		t.Fatal("GPT-4o should keep Chat Completions")
	}
}

func TestToolLoopDetectorStrategies(t *testing.T) {
	args := json.RawMessage(`{"tool_title":"check","command":"ls"}`)
	d := newToolLoopDetector()
	for i := 0; i < 9; i++ {
		if r := d.check("shell_execute", args); r.Level != "" {
			t.Fatalf("early check %d = %+v", i, r)
		}
		d.record("shell_execute", args, "same output", "")
	}
	if r := d.record("shell_execute", args, "same output", ""); r.Level != "warning" {
		t.Fatalf("10th call = %+v", r)
	}
	// No-progress circuit breaker at 30 identical outcomes.
	for i := 0; i < 20; i++ {
		d.record("shell_execute", args, "same output", "")
	}
	if r := d.check("shell_execute", args); r.Level != "critical" {
		t.Fatalf("circuit breaker = %+v", r)
	}

	u := newToolLoopDetector()
	for i := 0; i < 10; i++ {
		u.record("missing_tool", json.RawMessage(`{"x":1}`), "", "Unknown tool: missing_tool")
	}
	if r := u.check("missing_tool", json.RawMessage(`{"x":1}`)); r.Level != "critical" || !strings.Contains(r.Message, "unavailable tool") {
		t.Fatalf("unknown streak = %+v", r)
	}

	p := newToolLoopDetector()
	poll := json.RawMessage(`{"tool_title":"poll","action":"poll"}`)
	for i := 0; i < 10; i++ {
		p.record("process", poll, "still running", "")
	}
	if r := p.check("process", poll); r.Level != "warning" {
		t.Fatalf("poll warning = %+v", r)
	}
	for i := 0; i < 10; i++ {
		p.record("process", poll, "still running", "")
	}
	if r := p.check("process", poll); r.Level != "critical" {
		t.Fatalf("poll critical = %+v", r)
	}
}

func TestToolLoopDetectorHydrationAndExemption(t *testing.T) {
	var msgs []WireMessage
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			WireMessage{Role: "assistant", Parts: []WirePart{{Type: "tool_use", ID: id, Name: "shell_execute", Args: json.RawMessage(`{"command":"date"}`)}}},
			WireMessage{Role: "user", Parts: []WirePart{{Type: "tool_result", ID: id, Name: "shell_execute", Content: "same"}}},
		)
	}
	d := newToolLoopDetector()
	d.hydrate(msgs)
	if r := d.check("shell_execute", json.RawMessage(`{"command":"date"}`)); r.Level != "warning" {
		t.Fatalf("hydrated repeat = %+v", r)
	}
	s := newToolLoopDetector()
	s.exemptNextCall()
	if r := s.record("shell_execute", json.RawMessage(`{"command":"date"}`), "same", ""); r.Level != "" {
		t.Fatalf("scheduled exemption = %+v", r)
	}
}

func TestToolTitleReminder(t *testing.T) {
	d := newToolLoopDetector()
	for i := 0; i < 11; i++ {
		got := d.recordToolTitle(json.RawMessage(`{"command":"ls"}`), true)
		if i == 0 || i == 10 {
			if got == "" {
				t.Fatalf("reminder missing on call %d", i+1)
			}
		} else if got != "" {
			t.Fatalf("reminder repeated on call %d", i+1)
		}
	}
	if got := d.recordToolTitle(json.RawMessage(`{"tool_title":"x"}`), true); got != "" {
		t.Fatal("titled call should not be reminded")
	}
	if got := d.recordToolTitle(json.RawMessage(`{}`), false); got != "" {
		t.Fatal("untitled tool should not be reminded")
	}
}

func TestPreflightToolCall(t *testing.T) {
	tools := []ToolDef{
		{Type: "function", Function: ToolFunction{Name: "file_edit", Parameters: json.RawMessage(`{"type":"object","properties":{"tool_title":{},"path":{},"old_string":{},"new_string":{}},"required":["tool_title","path","old_string","new_string"]}`)}},
		{Type: "function", Function: ToolFunction{Name: "shell_execute", Parameters: json.RawMessage(`{"type":"object","properties":{"tool_title":{},"command":{}},"required":["tool_title","command"]}`)}},
	}
	if got := preflightToolCall("file_edit", json.RawMessage(`{"path":"/x","old_string":"a","new_string":""}`), tools); got != "" {
		t.Fatalf("empty new_string should be allowed: %s", got)
	}
	if got := preflightToolCall("file_edit", json.RawMessage(`{"path":"/x","old_string":"a"}`), tools); !strings.Contains(got, "new_string") {
		t.Fatalf("missing field = %q", got)
	}
	if got := preflightToolCall("shell_execute", json.RawMessage(`{"tool_title":"x","command":""}`), tools); !strings.Contains(got, "command") {
		t.Fatalf("empty command = %q", got)
	}
	if got := preflightToolCall("shell_execute", json.RawMessage(`{}`), tools); !strings.Contains(got, "requires: command") {
		t.Fatalf("empty args = %q", got)
	}
}

func TestAnthropicThinkingConfig(t *testing.T) {
	thinking, output := anthropicThinkingConfig("claude-sonnet-4-5", true, "high", 8192)
	if thinking == nil || thinking["type"] != "enabled" || thinking["budget_tokens"] != 8191 || output != nil {
		t.Fatalf("legacy thinking = %+v %+v", thinking, output)
	}
	thinking, output = anthropicThinkingConfig("claude-opus-4-7", true, "xhigh", 16000)
	if thinking == nil || thinking["type"] != "adaptive" || output["effort"] != "max" {
		t.Fatalf("adaptive thinking = %+v %+v", thinking, output)
	}
	thinking, output = anthropicThinkingConfig("claude-opus-4-7", true, "off", 16000)
	if thinking == nil || thinking["type"] != "disabled" {
		t.Fatalf("4.x off = %+v %+v", thinking, output)
	}
	thinking, _ = anthropicThinkingConfig("claude-sonnet-5-5", true, "off", 16000)
	if thinking == nil || thinking["type"] != "between_tools" {
		t.Fatalf("sonnet 5.5 off = %+v", thinking)
	}
	thinking, output = anthropicThinkingConfig("claude-opus-5", true, "off", 16000)
	if thinking == nil || thinking["type"] != "adaptive" || output["effort"] != "low" {
		t.Fatalf("opus 5 off = %+v %+v", thinking, output)
	}
	if thinking, _ = anthropicThinkingConfig("claude-sonnet-4-5", true, "off", 16000); thinking != nil {
		t.Fatalf("legacy off should omit thinking: %+v", thinking)
	}
	if thinking, _ = anthropicThinkingConfig("custom-model", false, "high", 16000); thinking != nil {
		t.Fatalf("non-reasoning model should omit thinking: %+v", thinking)
	}
}

func TestAnthropicRequestThinkingAndAuth(t *testing.T) {
	var body map[string]any
	var auth, key, beta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		key = r.Header.Get("x-api-key")
		beta = r.Header.Get("anthropic-beta")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()
	p := &anthropicProvider{cfg: &Provider{ID: "a", Kind: "anthropic", BaseURL: upstream.URL, APIKey: "sk-test"}, http: newProviderHTTP()}
	_, err := p.Stream(context.Background(), ProviderRequest{
		Model:    &Model{ID: "claude-sonnet-4-5", SupportsReasoning: true},
		Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "hi"}}}},
		Thinking: "high", MaxTokens: 8192,
	}, func(ProviderEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-test" || key != "" {
		t.Fatalf("custom endpoint auth = %q key=%q", auth, key)
	}
	if beta != "interleaved-thinking-2025-05-14" {
		t.Fatalf("beta = %q", beta)
	}
	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(8191) {
		t.Fatalf("request thinking = %+v", body["thinking"])
	}
}

func TestOpenAIReasoningOffAndClamp(t *testing.T) {
	if got := openAIReasoningEffortFor("https://api.openai.com/v1", &Model{ID: "gpt-5.6", SupportsReasoning: true}, "off"); got != "none" {
		t.Fatalf("official OpenAI off = %q", got)
	}
	if got := openAIReasoningEffortFor("https://api.openai.com/v1", &Model{ID: "gpt-6-astra", SupportsReasoning: true, ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}}, "off"); got != "low" {
		t.Fatalf("off should clamp to lowest declared tier: %q", got)
	}
	if got := openAIReasoningEffortFor("https://api.openai.com/v1", &Model{ID: "gpt-6", SupportsReasoning: true, ReasoningEfforts: []string{"low", "high"}}, "xhigh"); got != "high" {
		t.Fatalf("xhigh should clamp down: %q", got)
	}
	if got := openAIReasoningEffortFor("https://relay.example/v1", &Model{ID: "x", SupportsReasoning: true}, "off"); got != "" {
		t.Fatalf("unknown relay off should omit: %q", got)
	}
	if got := openAIReasoningEffortFor("https://ark.example/v1", &Model{ID: "doubao-x", SupportsReasoning: true}, "off"); got != "minimal" {
		t.Fatalf("ark off = %q", got)
	}
}

func TestProviderIdleTimeoutOverride(t *testing.T) {
	if got := (&Provider{}).idleTimeout(); got != providerIdleTimeout {
		t.Fatalf("default idle timeout = %s", got)
	}
	if got := (&Provider{IdleTimeoutSec: 120}).idleTimeout(); got != 2*time.Minute {
		t.Fatalf("configured idle timeout = %s", got)
	}
}

func TestBareAPIRunIsHeadless(t *testing.T) {
	_, ts := testServer(t, "mock")
	frames := runSSE(t, ts, RunRequest{
		SessionID: "sess-bare", RunID: "run-bare", Model: "mock-1",
		Tools:    []ToolDef{mockTool("memory_get")},
		Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "run [[client:memory_get]]"}}}},
	})
	for _, f := range frames {
		if f.Event == "await_client" {
			t.Fatal("bare API run waited for a device tool")
		}
		if f.Event == "tool_result" {
			if f.Data["is_error"] != true {
				t.Fatalf("headless tool result = %+v", f.Data)
			}
		}
	}
}

func TestTerminalIdleReaper(t *testing.T) {
	m := newTerminalManager(false, t.TempDir())
	now := time.Now()
	idle := &terminal{id: "idle", lastUsed: now.Add(-time.Hour), wake: make(chan struct{}, 1)}
	busy := &terminal{id: "busy", lastUsed: now.Add(-time.Hour), streams: 1, wake: make(chan struct{}, 1)}
	active := &terminal{id: "active", lastUsed: now, wake: make(chan struct{}, 1)}
	m.mu.Lock()
	m.terms[idle.id] = idle
	m.terms[busy.id] = busy
	m.terms[active.id] = active
	m.mu.Unlock()
	m.reapIdle(now)
	m.mu.Lock()
	_, idleStill := m.terms["idle"]
	_, busyStill := m.terms["busy"]
	_, activeStill := m.terms["active"]
	m.mu.Unlock()
	if idleStill || !busyStill || !activeStill {
		t.Fatalf("reaper result idle=%v busy=%v active=%v", idleStill, busyStill, activeStill)
	}
}

func TestHeadlessPromptHidesDeviceTools(t *testing.T) {
	prompt := buildSystemPrompt(&PromptContext{
		Headless: true, BrowserEnabled: true, UserAskEnabled: true, DelegationEnabled: true,
		MemoryEnabled: func() *bool { v := true; return &v }(),
	}, "/var/minis/workspace")
	for _, banned := range []string{"Android framework commands", "browser_use:", "memory_write:", "user_ask:", "subagent_task:"} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("headless prompt advertises %q", banned)
		}
	}
	if !strings.Contains(prompt, "Memory system (currently DISABLED)") {
		t.Fatal("headless prompt should say memory is unavailable")
	}
	tools := serverToolsOnly([]ToolDef{
		{Function: ToolFunction{Name: "shell_execute"}},
		{Function: ToolFunction{Name: "browser_use"}},
		{Function: ToolFunction{Name: "memory_get"}},
	})
	if len(tools) != 1 || tools[0].Function.Name != "shell_execute" {
		t.Fatalf("headless tools = %+v", tools)
	}
}

func TestGlobalStorePathsAreMapped(t *testing.T) {
	dir := t.TempDir()
	cfg := SandboxConfig{WorkRoot: filepath.Join(dir, "sessions"), GlobalRoot: filepath.Join(dir, "global")}
	sb, err := NewSandbox(cfg, "sess-global")
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"/var/minis/shared", "/var/minis/skills", "/var/minis/memory"} {
		path := root + "/demo.txt"
		if err := sb.Upload(path, []byte("global")); err != nil {
			t.Fatalf("upload %s: %v", path, err)
		}
		got, err := sb.ReadFile(path, 1, 0, 100)
		if err != nil || !strings.Contains(got, "global") {
			t.Fatalf("read %s = %q err=%v", path, got, err)
		}
		entries, err := sb.ListDir(root)
		if err != nil || len(entries) == 0 {
			t.Fatalf("list %s = %+v err=%v", root, entries, err)
		}
	}
	if err := sb.Upload("/var/minis/anything/escape.txt", []byte("x")); err == nil {
		t.Fatal("unknown global path should be confined")
	}
}

func TestTerminalSanitizer(t *testing.T) {
	raw := "\x1b[31mred\x1b[0m\r\nprogress 10%\rprogress 100%\n\n\nnull\nnullnull\nend\x00"
	got := sanitizeTerminalOutput(raw)
	if strings.Contains(got, "\x1b") || strings.ContainsAny(got, "\x00\r") {
		t.Fatalf("control bytes survived: %q", got)
	}
	if !strings.Contains(got, "progress 100%") || !strings.Contains(got, "end") {
		t.Fatalf("sanitized = %q", got)
	}
	if strings.Contains(got, "null") {
		t.Fatalf("null artifacts survived: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("blank run survived: %q", got)
	}
	long := strings.Repeat("a", 60_000)
	out := truncateMiddle(long, 50_000)
	if !strings.Contains(out, "10000 characters omitted") {
		t.Fatalf("truncation marker missing")
	}
	if got := formatShellResult("", 0, false); got != "(no output)" {
		t.Fatalf("empty = %q", got)
	}
	if got := formatShellResult("boom", 2, false); !strings.HasSuffix(got, "(exit code: 2)") {
		t.Fatalf("failure = %q", got)
	}
	if got := formatShellResult("slow", -1, true); !strings.HasSuffix(got, "(exit code: 124)") {
		t.Fatalf("timeout = %q", got)
	}
}

func TestReadFileDirectionAndTruncation(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(SandboxConfig{WorkRoot: dir, MaxOutput: 1000}, "sess-read")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("line %03d", i))
	}
	if err := sb.WriteFile("/var/minis/workspace/lines.txt", strings.Join(lines, "\n"), false, true); err != nil {
		t.Fatal(err)
	}
	head, err := sb.ReadFileDirection("/var/minis/workspace/lines.txt", 1, 5, 1000, "head")
	if err != nil || !strings.Contains(head, "showing 1-5 of 100") || !strings.Contains(head, "line 001") || strings.Contains(head, "line 006") {
		t.Fatalf("head = %q err=%v", head, err)
	}
	tail, err := sb.ReadFileDirection("/var/minis/workspace/lines.txt", 1, 5, 1000, "tail")
	if err != nil || !strings.Contains(tail, "showing 96-100 of 100") || !strings.Contains(tail, "line 100") || strings.Contains(tail, "line 095") {
		t.Fatalf("tail = %q err=%v", tail, err)
	}
	truncated, err := sb.ReadFileDirection("/var/minis/workspace/lines.txt", 1, 0, 50, "head")
	if err != nil || !strings.Contains(truncated, "truncated at 50 chars") || !strings.Contains(truncated, "next_offset=") {
		t.Fatalf("truncated = %q err=%v", truncated, err)
	}
}

func TestImageBounds(t *testing.T) {
	// Minimal PNG header with 2048x1024 IHDR dimensions.
	png := make([]byte, 24)
	copy(png, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	binary.BigEndian.PutUint32(png[16:20], 2048)
	binary.BigEndian.PutUint32(png[20:24], 1024)
	if w, h := imageBounds(png); w != 2048 || h != 1024 {
		t.Fatalf("png bounds = %dx%d", w, h)
	}
	gif := []byte("GIF89a\x40\x01\xf0\x00")
	if w, h := imageBounds(gif); w != 320 || h != 240 {
		t.Fatalf("gif bounds = %dx%d", w, h)
	}
	if w, h := imageBounds([]byte("not an image")); w != 0 || h != 0 {
		t.Fatalf("unknown bounds = %dx%d", w, h)
	}
}

func TestAzureOpenAIRequest(t *testing.T) {
	var path, key, auth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		key = r.Header.Get("api-key")
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer upstream.Close()
	p := &openAIProvider{cfg: &Provider{
		ID: "azure", Kind: "openai", BaseURL: upstream.URL + "?api-version=2025-01-01", APIKey: "az", Azure: true,
	}, http: newProviderHTTP()}
	_, err := p.Stream(context.Background(), ProviderRequest{
		Model: &Model{ID: "gpt-5-deploy"}, Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "hi"}}}},
	}, func(ProviderEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if path != "/openai/deployments/gpt-5-deploy/chat/completions" {
		t.Fatalf("azure path = %q", path)
	}
	if key != "az" || auth != "" {
		t.Fatalf("azure auth key=%q auth=%q", key, auth)
	}
}

func TestAnthropicToolIDMapping(t *testing.T) {
	wire := anthropicWireID("call_abc|fc_def")
	if !anthropicIDPattern.MatchString(wire) || wire != anthropicWireID("call_abc|fc_def") {
		t.Fatalf("mapped id = %q", wire)
	}
	if anthropicWireID("toolu_123") != "toolu_123" {
		t.Fatal("valid id changed")
	}
	msgs := []WireMessage{
		{Role: "assistant", Parts: []WirePart{{Type: "tool_use", ID: "call_abc|fc_def", Name: "x", Args: json.RawMessage(`{}`)}}},
		{Role: "user", Parts: []WirePart{{Type: "tool_result", ID: "call_abc|fc_def", Name: "x", Content: "ok"}}},
	}
	out, err := anthropicMessages(msgs, false)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprint(out)
	if strings.Contains(raw, "|") || !strings.Contains(raw, wire) {
		t.Fatalf("wire history = %s", raw)
	}
}

func TestOpenAIUsageCacheAccounting(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_cache_hit_tokens":60,"prompt_tokens_details":{"cache_write_tokens":10},"completion_tokens_details":{"reasoning_tokens":3}}}`+"\n\n")
	}))
	defer upstream.Close()
	p := &openAIProvider{cfg: &Provider{Kind: "openai", BaseURL: upstream.URL, APIKey: "k"}, http: newProviderHTTP()}
	res, err := p.Stream(context.Background(), ProviderRequest{
		Model: &Model{ID: "m"}, Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "hi"}}}},
	}, func(ProviderEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.InputTokens != 30 || res.Usage.CacheReadInputTokens != 60 || res.Usage.CacheCreationInputTokens != 10 || res.Usage.LatestContextTokens != 100 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestGeminiUsageAccounting(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":20,"candidatesTokenCount":10,"thoughtsTokenCount":5}}`+"\n\n")
	}))
	defer upstream.Close()
	p := &geminiProvider{cfg: &Provider{Kind: "gemini", BaseURL: upstream.URL, APIKey: "k"}, http: newProviderHTTP()}
	res, err := p.Stream(context.Background(), ProviderRequest{
		Model: &Model{ID: "gemini-3-pro"}, Messages: []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: "hi"}}}},
	}, func(ProviderEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.InputTokens != 80 || res.Usage.OutputTokens != 15 || res.Usage.CacheReadInputTokens != 20 || res.Usage.LatestContextTokens != 100 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestContextOffloadHistory(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(SandboxConfig{WorkRoot: dir, MaxOutput: 1_000_000}, "sess-offload-history")
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("output line\n", 8000) // ~48 KB
	var msgs []WireMessage
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			WireMessage{Role: "assistant", Parts: []WirePart{{Type: "tool_use", ID: id, Name: "shell_execute", Args: json.RawMessage(`{"command":"x"}`)}}},
			WireMessage{Role: "user", Parts: []WirePart{{Type: "tool_result", ID: id, Name: "shell_execute", Content: big}}},
		)
	}
	msgs = append(msgs, WireMessage{Role: "user", Parts: []WirePart{{Type: "text", Text: "recent"}}})
	s := &Server{}
	out, n := s.offloadHistory(sb, msgs, "system", nil, 128_000)
	if n == 0 {
		t.Fatal("nothing was offloaded")
	}
	if !strings.HasPrefix(out[1].Parts[0].Content, offloadedPrefix) {
		t.Fatalf("old result not stubbed: %q", out[1].Parts[0].Content[:20])
	}
	last := len(out) - 4
	for i := last; i < len(out); i++ {
		for _, p := range out[i].Parts {
			if strings.HasPrefix(p.Content, offloadedPrefix) {
				t.Fatalf("protected message %d was offloaded", i)
			}
		}
	}
	entries, err := sb.ListDir("/var/minis/offloads/tools")
	if err != nil || len(entries) == 0 {
		t.Fatalf("offload files = %+v err=%v", entries, err)
	}
}
