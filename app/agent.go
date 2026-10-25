package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type Run struct {
	ID     string
	ctx    context.Context
	cancel context.CancelFunc
	events chan Event
	closed sync.Once

	pmu     sync.Mutex
	pending map[string]chan ToolResult
}

func newRun(parent context.Context, id string) *Run {
	ctx, cancel := context.WithCancel(parent)
	return &Run{
		ID:      id,
		ctx:     ctx,
		cancel:  cancel,
		events:  make(chan Event, 256),
		pending: map[string]chan ToolResult{},
	}
}

func (r *Run) emit(name string, data any) bool {
	select {
	case <-r.ctx.Done():
		return false
	case r.events <- Event{Name: name, Data: data}:
		return true
	}
}

func (r *Run) close() {
	r.closed.Do(func() { close(r.events) })
}

func (r *Run) beginClientTool(id string) chan ToolResult {
	ch := make(chan ToolResult, 1)
	r.pmu.Lock()
	r.pending[id] = ch
	r.pmu.Unlock()
	return ch
}

func (r *Run) finishClientTool(id string) {
	r.pmu.Lock()
	delete(r.pending, id)
	r.pmu.Unlock()
}

func (r *Run) deliverClientTool(res ToolResult) error {
	r.pmu.Lock()
	ch := r.pending[res.ID]
	r.pmu.Unlock()
	if ch == nil {
		return errors.New("no tool call is waiting for that id")
	}
	select {
	case ch <- res:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

func (r *Run) waitClientTool(ctx context.Context, id string, timeout time.Duration) (ToolResult, error) {
	ch := r.beginClientTool(id)
	defer r.finishClientTool(id)
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	case <-time.After(timeout):
		return ToolResult{}, errors.New("timed out waiting for the Android client to return the tool result")
	}
}

func (r *Run) waitClientToolCh(ctx context.Context, ch chan ToolResult, timeout time.Duration) (ToolResult, error) {
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	case <-time.After(timeout):
		return ToolResult{}, errors.New("timed out waiting for the Android client to return the tool result")
	}
}

func (s *Server) registerRun(r *Run) {
	s.runsMu.Lock()
	if old := s.runs[r.ID]; old != nil {
		old.cancel()
	}
	s.runs[r.ID] = r
	s.runsMu.Unlock()
}

func (s *Server) unregisterRun(r *Run) {
	s.runsMu.Lock()
	if s.runs[r.ID] == r {
		delete(s.runs, r.ID)
	}
	s.runsMu.Unlock()
	r.cancel()
}

func (s *Server) cancelRun(id string) bool {
	s.runsMu.Lock()
	r := s.runs[id]
	s.runsMu.Unlock()
	if r == nil {
		return false
	}
	r.cancel()
	return true
}

func (s *Server) cancelAllRuns() {
	s.runsMu.Lock()
	for _, r := range s.runs {
		r.cancel()
	}
	s.runsMu.Unlock()
}

func (s *Server) providerFor(p *Provider) ProviderClient {
	switch p.Kind {
	case "anthropic":
		return &anthropicProvider{cfg: p, http: s.http}
	case "mock":
		return &mockProvider{}
	default:
		return &openAIProvider{cfg: p, http: s.http}
	}
}

func (s *Server) runAgent(req RunRequest, run *Run) {
	defer run.close()

	sessionID := strings.TrimSpace(req.SessionID)
	if !sessionIDPattern.MatchString(sessionID) {
		run.emit("error", map[string]any{"message": "invalid or missing session_id"})
		return
	}
	if _, err := s.store.EnsureSession(sessionID, req.Model); err != nil {
		run.emit("error", map[string]any{"message": "session store: " + err.Error()})
		return
	}
	provider, model, err := s.cfg.ResolveModel(req.Model)
	if err != nil {
		run.emit("error", map[string]any{"message": err.Error()})
		return
	}
	providerClient := s.providerFor(provider)

	sb, err := NewToolSandbox(s.cfg.Sandbox, sessionID)
	if err != nil {
		run.emit("error", map[string]any{"message": "sandbox: " + err.Error()})
		return
	}

	maxTurns := req.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}
	if maxTurns > 30 {
		maxTurns = 30
	}
	maxTokens := req.MaxTokens

	tools := s.buildTools(req.Tools)
	system := strings.TrimSpace(req.System)
	if system == "" {
		system = "You are Minis, a capable AI assistant running on your user's personal OpenMinis server. Be direct, accurate and useful. Use tools when they are needed."
	}
	system += "\n\n<server-runtime>\n" +
		"Shell and file tools run inside a dedicated full Linux distribution container (Debian), not PRoot/Alpine and not on the phone. " +
		"The shell runs as root, so distro package management (apt-get/apt) is available; use it when a task needs system tools. " +
		"The session workspace is " + sb.Workspace() + ". The client's virtual paths /var/minis/workspace and /var/minis/attachments are mapped into it automatically. " +
		"Browser automation, device integrations, memory, skills and user questions are executed by the paired Android client when they appear as tool calls. " +
		"Do not claim you are on the phone; work in the server's Linux container.\n</server-runtime>"

	// Store the user's new prompt exactly once. History is client-owned, so
	// the server only needs this row for future Web UI session browsing.
	if uid, text := newUserMessage(req.Messages); uid != "" || text != "" {
		id := strings.TrimSpace(req.UserMessageID)
		if id == "" {
			id = newID("user")
		}
		msg := StoredMessage{
			ID:        id,
			SessionID: sessionID,
			Role:      "user",
			Parts:     []WirePart{{Type: "text", Text: text}},
			CreatedAt: nowMillis(),
		}
		if _, err := s.store.AppendMessage(msg); err != nil {
			log.Printf("store user message: %v", err)
		}
	}

	messages := append([]WireMessage(nil), req.Messages...)
	run.emit("ready", map[string]any{
		"run_id":     run.ID,
		"session_id": sessionID,
		"model":      model.ID,
		"provider":   provider.ID,
	})

	for turn := 0; turn < maxTurns; turn++ {
		if run.ctx.Err() != nil {
			return
		}
		parts := []*WirePart{}
		var textBuilder strings.Builder
		var thinkingBuf strings.Builder
		textOpen := false
		type pendingCall struct {
			part      *WirePart
			args      strings.Builder
			completed bool
		}
		calls := map[string]*pendingCall{}
		callOrder := []string{}
		var usage Usage
		hasTool := false

		appendText := func(delta string) {
			if delta == "" {
				return
			}
			if textOpen && len(parts) > 0 && parts[len(parts)-1].Type == "text" {
				parts[len(parts)-1].Text += delta
			} else {
				parts = append(parts, &WirePart{Type: "text", Text: delta})
				textOpen = true
			}
			textBuilder.WriteString(delta)
			run.emit("text", map[string]any{"text": delta})
		}
		ensureCall := func(id, name string) *pendingCall {
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", turn, len(callOrder))
			}
			if c := calls[id]; c != nil {
				if name != "" && c.part.Name == "" {
					c.part.Name = name
				}
				return c
			}
			part := &WirePart{Type: "tool_use", ID: id, Name: name, Args: json.RawMessage(`{}`)}
			parts = append(parts, part)
			textOpen = false
			c := &pendingCall{part: part}
			calls[id] = c
			callOrder = append(callOrder, id)
			hasTool = true
			executor := "client"
			if serverToolNames[name] {
				executor = "server"
				// File tools may be executed by either side depending on the
				// path, which is only known once the arguments are complete.
				if name == "file_read" || name == "file_write" || name == "file_edit" {
					executor = "pending"
				}
			}
			run.emit("tool_start", map[string]any{"id": id, "name": name, "executor": executor})
			return c
		}

		pres, streamErr := providerClient.Stream(run.ctx, ProviderRequest{
			Model:     model,
			System:    system,
			Messages:  messages,
			Tools:     tools,
			MaxTokens: maxTokens,
			Thinking:  req.Thinking,
		}, func(ev ProviderEvent) error {
			switch ev.Kind {
			case "text":
				appendText(ev.Text)
			case "thinking":
				thinkingBuf.WriteString(ev.Text)
				run.emit("thinking", map[string]any{"text": ev.Text})
			case "tool_start":
				ensureCall(ev.ID, ev.Name)
			case "tool_input":
				c := ensureCall(ev.ID, ev.Name)
				c.args.WriteString(ev.Args)
				c.part.Args = json.RawMessage(`{}`)
				run.emit("tool_input", map[string]any{"id": c.part.ID, "delta": ev.Args})
			case "tool_call":
				c := ensureCall(ev.ID, ev.Name)
				args := strings.TrimSpace(ev.Args)
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					args = "{}"
				}
				c.part.Args = json.RawMessage(args)
				c.completed = true
				executor := "client"
				if serverToolNames[c.part.Name] && !clientOwnedCall(c.part.Name, c.part.Args) {
					executor = "server"
				}
				var argObj any
				_ = json.Unmarshal([]byte(args), &argObj)
				run.emit("tool_call", map[string]any{
					"id":       c.part.ID,
					"name":     c.part.Name,
					"args":     argObj,
					"executor": executor,
				})
			}
			return nil
		})
		if streamErr != nil {
			if run.ctx.Err() != nil {
				return
			}
			// Keep any text that streamed before the failure; drop incomplete
			// tool calls so the transcript never contains an unpaired tool_use.
			clean := make([]WirePart, 0, len(parts))
			for _, p := range parts {
				if p.Type == "tool_use" {
					continue
				}
				clean = append(clean, *p)
			}
			if len(clean) > 0 {
				run.emit("assistant_end", map[string]any{"parts": clean, "stop_reason": "error", "usage": usage, "reasoning_content": thinkingBuf.String()})
				id := newID("asst")
				_, _ = s.store.AppendMessage(StoredMessage{ID: id, SessionID: sessionID, Role: "assistant", Parts: clean, CreatedAt: nowMillis()})
			}
			run.emit("error", map[string]any{"message": streamErr.Error()})
			return
		}
		usage = pres.Usage
		if usage.LatestContextTokens == 0 {
			usage.LatestContextTokens = usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
		}
		run.emit("usage", usage)

		// A tool_use part whose call never completed is unusable. Remove it
		// before announcing the assistant turn.
		finalParts := make([]WirePart, 0, len(parts))
		validIDs := map[string]bool{}
		for _, p := range parts {
			if p.Type == "tool_use" {
				c := calls[p.ID]
				if c == nil {
					continue
				}
				if !c.completed && c.args.Len() > 0 {
					raw := strings.TrimSpace(c.args.String())
					if json.Valid([]byte(raw)) {
						c.part.Args = json.RawMessage(raw)
						c.completed = true
					}
				}
				if !c.completed {
					continue
				}
				validIDs[p.ID] = true
				finalParts = append(finalParts, *c.part)
				continue
			}
			finalParts = append(finalParts, *p)
		}
		orderedIDs := make([]string, 0, len(callOrder))
		for _, id := range callOrder {
			if validIDs[id] {
				orderedIDs = append(orderedIDs, id)
			}
		}
		hasTool = len(orderedIDs) > 0
		stop := pres.StopReason
		if hasTool {
			stop = "tool_use"
		}
		if stop == "" {
			stop = "end_turn"
		}
		if len(finalParts) == 0 {
			run.emit("error", map[string]any{"message": "the model returned an empty response"})
			return
		}
		reasoning := thinkingBuf.String()
		run.emit("assistant_end", map[string]any{
			"parts":             finalParts,
			"stop_reason":       stop,
			"usage":             usage,
			"reasoning_content": reasoning,
		})
		assistantID := newID("asst")
		if _, err := s.store.AppendMessage(StoredMessage{
			ID: assistantID, SessionID: sessionID, Role: "assistant",
			Parts: finalParts, ReasoningContent: reasoning, Usage: &usage, CreatedAt: nowMillis(),
		}); err != nil {
			log.Printf("store assistant message: %v", err)
		}

		if !hasTool {
			run.emit("done", map[string]any{"stop_reason": stop, "turns": turn + 1})
			return
		}

		// Execute the batch in provider order. Server tools run here; client
		// tools pause the run until the Android app posts their result.
		results := make([]ToolResult, 0, len(orderedIDs))
		for _, id := range orderedIDs {
			call := calls[id]
			name := call.part.Name
			args := call.part.Args
			if serverToolNames[name] && !clientOwnedCall(name, args) {
				run.emit("tool_running", map[string]any{"id": id, "name": name})
				res := s.executeTool(run.ctx, sb, name, args, func(line string) {
					run.emit("tool_output", map[string]any{"id": id, "line": line})
				})
				res.ID = id
				res.Executor = "server"
				run.emit("tool_result", res)
				results = append(results, res)
				continue
			}
			// Client-owned tool. Register the result channel BEFORE the
			// await event so a fast POST can never arrive before we listen.
			ch := run.beginClientTool(id)
			run.emit("await_client", map[string]any{"id": id, "name": name})
			res, err := run.waitClientToolCh(run.ctx, ch, 65*time.Minute)
			run.finishClientTool(id)
			if err != nil {
				if run.ctx.Err() != nil {
					return
				}
				res = ToolResult{
					ID: id, Name: name, IsError: true, Executor: "client",
					Content: "Error: " + err.Error(),
				}
			}
			if res.Name == "" {
				res.Name = name
			}
			res.ID = id
			res.Executor = "client"
			run.emit("tool_result", res)
			results = append(results, res)
		}

		run.emit("tools_end", map[string]any{"results": results})
		resultParts := make([]WirePart, 0, len(results))
		durations := map[string]int64{}
		for _, r := range results {
			resultParts = append(resultParts, WirePart{
				Type: "tool_result", ID: r.ID, Name: r.Name, Content: r.Content, IsError: r.IsError,
			})
			if r.DurationMs > 0 {
				durations[r.ID] = r.DurationMs
			}
		}
		if _, err := s.store.AppendMessage(StoredMessage{
			ID: newID("tool"), SessionID: sessionID, Role: "user", Parts: resultParts,
			DurationsMs: durations, CreatedAt: nowMillis(),
		}); err != nil {
			log.Printf("store tool result: %v", err)
		}

		assistantMsg := WireMessage{Role: "assistant", Parts: finalParts, ReasoningContent: reasoning}
		messages = append(messages, assistantMsg)
		messages = append(messages, WireMessage{Role: "user", Parts: resultParts})
	}
	run.emit("error", map[string]any{"message": fmt.Sprintf("maximum agent turns reached (%d)", maxTurns)})
	run.emit("done", map[string]any{"stop_reason": "max_turns", "turns": maxTurns})
}

func newUserMessage(msgs []WireMessage) (id, text string) {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != "user" {
			continue
		}
		hasResult := false
		var b strings.Builder
		for _, p := range m.Parts {
			if p.Type == "tool_result" {
				hasResult = true
			}
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		if !hasResult {
			return "", b.String()
		}
	}
	return "", ""
}
