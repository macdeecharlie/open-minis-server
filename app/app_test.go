package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	s := NewServer(cfg, store)
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
	s := NewServer(cfg, store)
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
