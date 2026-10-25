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
	ID       string
	Session  string
	Headless bool
	ctx      context.Context
	cancel   context.CancelFunc

	pmu     sync.Mutex
	pending map[string]chan ToolResult

	smu    sync.Mutex
	steers []WireMessage

	logMu sync.Mutex
	log   []Event
	base  int // sequence number of log[0]; starts at 1
	done  bool
	wake  chan struct{}
}

func newRun(parent context.Context, id string) *Run {
	ctx, cancel := context.WithCancel(parent)
	return &Run{
		ID:      id,
		ctx:     ctx,
		cancel:  cancel,
		pending: map[string]chan ToolResult{},
		wake:    make(chan struct{}, 1),
		base:    1,
	}
}

// emit records an event and wakes every subscriber. The log is the source of
// truth for reconnects, so a slow client can never make the agent drop an
// event or block.
func (r *Run) emit(name string, data any) bool {
	if r.ctx.Err() != nil {
		return false
	}
	r.logMu.Lock()
	if len(r.log) == 0 {
		r.base = 1
	}
	r.log = append(r.log, Event{Name: name, Data: data})
	r.logMu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return true
}

// subscribe replays events after sequence number `after` (1-based), then waits
// for new ones until the run finishes or ctx is cancelled.
func (r *Run) subscribe(ctx context.Context, after int, fn func(seq int, ev Event) bool) {
	for {
		r.logMu.Lock()
		start := after - r.base + 1
		if start < 0 {
			start = 0
		}
		var batch []Event
		if start < len(r.log) {
			batch = append([]Event(nil), r.log[start:]...)
		}
		done := r.done
		base := r.base
		r.logMu.Unlock()
		for i, ev := range batch {
			seq := base + start + i
			if !fn(seq, ev) {
				return
			}
			after = seq
		}
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
	}
}

func (r *Run) close() {
	r.logMu.Lock()
	if r.done {
		r.logMu.Unlock()
		return
	}
	r.done = true
	r.logMu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	r.cancel()
}

func (r *Run) finished() bool {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	return r.done
}

func (r *Run) addSteer(msg WireMessage) {
	r.smu.Lock()
	r.steers = append(r.steers, msg)
	r.smu.Unlock()
}

func (r *Run) takeSteers() []WireMessage {
	r.smu.Lock()
	defer r.smu.Unlock()
	out := r.steers
	r.steers = nil
	return out
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
	case "gemini":
		return &geminiProvider{cfg: p, http: s.http}
	case "mock":
		return &mockProvider{}
	default:
		return &openAIProvider{cfg: p, http: s.http}
	}
}

// offloadToolResult spills an oversized tool result into the session's
// offloads directory and replaces it with a short pointer, matching the
// Android agent's context offload so one large output cannot consume the
// model window.
func offloadToolResult(sb ToolSandbox, res *ToolResult) {
	const threshold = 40 * 1024
	if res == nil || res.IsError || len(res.Content) <= threshold {
		return
	}
	name := "tool_" + strings.NewReplacer("/", "_", ":", "_").Replace(res.ID) + ".txt"
	path := "/var/minis/offloads/" + name
	if err := sb.WriteFile(path, res.Content, false, true); err != nil {
		return
	}
	res.Content = fmt.Sprintf("[Large tool result saved to %s (%d bytes). Use file_read to inspect it.]", path, len(res.Content))
}

func (s *Server) runAgent(req RunRequest, run *Run) {
	defer run.close()

	sessionID := strings.TrimSpace(req.SessionID)
	if !sessionIDPattern.MatchString(sessionID) {
		run.emit("error", map[string]any{"message": "invalid or missing session_id"})
		return
	}
	run.Session = sessionID
	if _, err := s.store.EnsureSession(sessionID, req.Model); err != nil {
		run.emit("error", map[string]any{"message": "session store: " + err.Error()})
		return
	}
	provider, model, err := s.cfg.ResolveModel(req.Model)
	if err != nil {
		run.emit("error", map[string]any{"message": err.Error()})
		return
	}
	type modelAttempt struct {
		provider *Provider
		model    *Model
		client   ProviderClient
	}
	attempts := []modelAttempt{{provider: provider, model: model, client: s.providerFor(provider)}}
	for _, id := range req.Fallbacks {
		if id == "" || id == model.ID {
			continue
		}
		fp, fm, ferr := s.cfg.ResolveModel(id)
		if ferr != nil {
			continue
		}
		duplicate := false
		for _, a := range attempts {
			if a.model.ID == fm.ID {
				duplicate = true
				break
			}
		}
		if !duplicate {
			attempts = append(attempts, modelAttempt{provider: fp, model: fm, client: s.providerFor(fp)})
		}
	}

	sb, err := NewToolSandbox(s.cfg.Sandbox, sessionID)
	if err != nil {
		run.emit("error", map[string]any{"message": "sandbox: " + err.Error()})
		return
	}

	maxTurns := req.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}
	if maxTurns > 500 {
		maxTurns = 500
	}
	maxTokens := req.MaxTokens

	tools := s.buildTools(req.Tools)
	system := ""
	switch {
	case req.Prompt != nil:
		// The client sends structured state; the server owns prompt wording.
		system = buildSystemPrompt(req.Prompt, sb.Workspace())
	case strings.TrimSpace(req.System) != "":
		// Legacy clients still send a complete prompt. Keep it byte-for-byte
		// and only add the deployment note so the model knows the execution
		// environment changed.
		system = strings.TrimSpace(req.System) + "\n\n<server-runtime>\n" +
			"Shell and file tools run inside a dedicated full Linux distribution container (Debian), not PRoot/Alpine and not on the phone. " +
			"The shell runs as root, so distro package management (apt-get/apt) is available; use it when a task needs system tools. " +
			"The session workspace is " + sb.Workspace() + ". The client's virtual paths /var/minis/workspace and /var/minis/attachments are mapped into it automatically. " +
			"Browser automation, device integrations, memory, skills and user questions are executed by the paired Android client when they appear as tool calls. " +
			"Do not claim you are on the phone; work in the server's Linux container.\n</server-runtime>"
	default:
		// Headless runs (scheduled tasks) and bare API clients get the same
		// server-owned prompt with no session-specific fragments.
		system = buildSystemPrompt(nil, sb.Workspace())
	}

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
	if summary, anchor := s.store.Summary(sessionID); summary != "" {
		if applied, ok := applyStoredCompaction(messages, summary, anchor); ok {
			messages = applied
		}
	}
	if budgeted, dropped := applyImageBudget(messages); dropped > 0 {
		messages = budgeted
		run.emit("info", map[string]any{"text": fmt.Sprintf("%d older image(s) were omitted to stay within the request image budget.", dropped)})
	}
	run.emit("ready", map[string]any{
		"run_id":     run.ID,
		"session_id": sessionID,
		"model":      model.ID,
		"provider":   provider.ID,
	})

	startingAttempt := 0
	emptyNudges := 0
	inLoopCompactions := 0
	lastToolSignature := ""
	repeatedToolCalls := 0
	for turn := 0; turn < maxTurns; turn++ {
		if run.ctx.Err() != nil {
			return
		}
		compactProvider := attempts[startingAttempt]
		compactedMessages, didCompact, cerr := s.maybeCompact(
			run, messages, system, tools, compactProvider.model.ContextWindow,
			compactProvider.client, compactProvider.model, maxTokens, &inLoopCompactions,
		)
		if cerr != nil {
			run.emit("error", map[string]any{"message": cerr.Error()})
			return
		}
		if didCompact {
			if budgeted, dropped := applyImageBudget(compactedMessages); dropped > 0 {
				messages = budgeted
				run.emit("info", map[string]any{"text": fmt.Sprintf("%d older image(s) were omitted to stay within the request image budget.", dropped)})
			} else {
				messages = compactedMessages
			}
		}
		if steers := run.takeSteers(); len(steers) > 0 {
			messages = append(messages, steers...)
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

		var pres ProviderResult
		var streamErr error
		chosenAttempt := startingAttempt
		for attemptIdx := startingAttempt; attemptIdx < len(attempts); attemptIdx++ {
			attempt := attempts[attemptIdx]
			emitted := false
			for retry := 0; retry < 3; retry++ {
				emitted = false
				pres, streamErr = attempt.client.Stream(run.ctx, ProviderRequest{
					Model:     attempt.model,
					System:    system,
					Messages:  messages,
					Tools:     tools,
					MaxTokens: maxTokens,
					Thinking:  req.Thinking,
				}, func(ev ProviderEvent) error {
					emitted = true
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
				if streamErr == nil {
					chosenAttempt = attemptIdx
					break
				}
				if run.ctx.Err() != nil {
					return
				}
				if emitted {
					break
				}
				if streamErrorRetryable(streamErr) && retry < 2 {
					select {
					case <-run.ctx.Done():
						return
					case <-time.After(time.Duration(1<<retry) * time.Second):
					}
					continue
				}
				break
			}
			if streamErr == nil {
				break
			}
			if emitted || !streamErrorFallbackable(streamErr, req.FallbackStrategy) {
				break
			}
		}
		startingAttempt = chosenAttempt
		if streamErr == nil && chosenAttempt > 0 {
			run.emit("info", map[string]any{
				"text": "⚠️ " + attempts[0].model.ID + " failed; switched to " + attempts[chosenAttempt].model.ID,
			})
		}
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
		if len(orderedIDs) == 1 {
			call := calls[orderedIDs[0]]
			sig := call.part.Name + "|" + string(call.part.Args)
			if sig == lastToolSignature {
				repeatedToolCalls++
			} else {
				lastToolSignature = sig
				repeatedToolCalls = 0
			}
			if repeatedToolCalls == 2 {
				run.emit("info", map[string]any{"text": "The model is repeating the same tool call (" + call.part.Name + "). It will be stopped if the loop continues."})
			}
			if repeatedToolCalls >= 4 {
				run.emit("error", map[string]any{"message": "tool loop detected: " + call.part.Name + " was called with identical arguments five times"})
				return
			}
		} else {
			lastToolSignature = ""
			repeatedToolCalls = 0
		}
		stop := pres.StopReason
		if hasTool {
			stop = "tool_use"
		}
		if stop == "" {
			stop = "end_turn"
		}
		if len(finalParts) == 0 {
			// The official loop nudges an empty turn once instead of ending
			// the run with a blank bubble. Retry with a user-side reminder and
			// no tools forced; the second empty turn is a real failure.
			if emptyNudges < 1 {
				emptyNudges++
				messages = append(messages, WireMessage{
					Role:  "user",
					Parts: []WirePart{{Type: "text", Text: "<system-reminder>The previous response was empty. Continue the task or give the user a concise answer; do not return an empty response.</system-reminder>"}},
				})
				continue
			}
			run.emit("error", map[string]any{"message": "the model returned an empty response twice"})
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
				offloadToolResult(sb, &res)
				run.emit("tool_result", res)
				results = append(results, res)
				continue
			}
			// Client-owned tool. A scheduled/headless run has no paired app
			// to execute it; return a visible tool error rather than waiting
			// for a tool result that can never arrive.
			if run.Headless {
				res := ToolResult{
					ID: id, Name: name, Title: "Device tool unavailable", Executor: "client",
					IsError: true,
					Content: "Error: this tool requires the paired Android app, which is not connected for this scheduled run. Use server shell/file/web tools instead.",
				}
				run.emit("tool_result", res)
				results = append(results, res)
				continue
			}
			// Register the result channel BEFORE the await event so a fast POST
			// can never arrive before we listen.
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
			offloadToolResult(sb, &res)
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
