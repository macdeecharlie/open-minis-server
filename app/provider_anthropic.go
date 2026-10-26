package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type anthropicProvider struct {
	cfg  *Provider
	http *providerHTTP
}

func (p *anthropicProvider) Stream(ctx context.Context, req ProviderRequest, emit func(ProviderEvent) error) (ProviderResult, error) {
	msgs, err := anthropicMessages(req.Messages, anthropicEchoesUnsignedThinking(p.cfg.BaseURL, req.Model))
	if err != nil {
		return ProviderResult{}, err
	}
	maxTokens := effectiveMaxTokens(req.Model, req.MaxTokens)
	body := map[string]any{
		"model":      req.Model.ID,
		"max_tokens": maxTokens,
		"messages":   msgs,
		"stream":     true,
	}
	// Mirror the official thinking contract. Legacy Claude models take an
	// explicit token budget; Claude 4.6+ take adaptive thinking plus an
	// output-config effort. OFF is encoded per model family,
	// because an absent field is not off on Claude 5.
	if thinking, outputConfig := anthropicThinkingConfig(req.Model.ID, req.Model.SupportsReasoning, req.Thinking, maxTokens); thinking != nil {
		body["thinking"] = thinking
		if outputConfig != nil {
			body["output_config"] = outputConfig
		}
	}
	if req.Model.Temperature != nil && !thinkingEnabled(req.Thinking) && !anthropicUsesAdaptiveThinking(req.Model.ID) {
		body["temperature"] = *req.Model.Temperature
	}
	if req.Model.TopP != nil && !thinkingEnabled(req.Thinking) {
		body["top_p"] = *req.Model.TopP
	}
	system := req.System
	if prefix := strings.TrimSpace(p.cfg.SystemPrefix); prefix != "" && !strings.HasPrefix(system, prefix) {
		system = prefix + "\n\n" + system
	}
	if strings.TrimSpace(system) != "" {
		// Cache the stable prompt head (identity, tool rules, skills). The
		// runtime suffix is part of the same block; Anthropic caches the
		// longest matching prefix across requests, which is exactly the
		// Android prompt layout.
		body["system"] = []map[string]any{{
			"type": "text", "text": system,
			"cache_control": map[string]any{"type": "ephemeral"},
		}}
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for i, t := range req.Tools {
			tool := map[string]any{
				"name":         t.Function.Name,
				"description":  t.Function.Description,
				"input_schema": t.Function.Parameters,
			}
			if i == len(req.Tools)-1 {
				tool["cache_control"] = map[string]any{"type": "ephemeral"}
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
	}
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	custom := !strings.Contains(strings.ToLower(p.cfg.BaseURL), "api.anthropic.com")
	if p.cfg.APIKey != "" {
		if custom || strings.EqualFold(p.cfg.AuthType, "bearer") {
			headers["Authorization"] = "Bearer " + p.cfg.APIKey
		} else {
			headers["x-api-key"] = p.cfg.APIKey
		}
	}
	if req.Model.SupportsReasoning && thinkingEnabled(req.Thinking) {
		if anthropicUsesAdaptiveThinking(req.Model.ID) {
			headers["anthropic-beta"] = "effort-2025-11-24"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}
	for k, v := range p.cfg.Headers {
		headers[sanitizeProviderHeaderValue(k)] = sanitizeProviderHeaderValue(v)
	}
	headers = applyModelHeaders(headers, req.Model)
	mergeExtraBody(body, req.Model)
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	resp, err := p.http.doJSON(streamCtx, "POST", anthropicEndpoint(p.cfg.BaseURL), headers, body, true)
	if err != nil {
		return ProviderResult{}, err
	}
	steadyIdle := p.cfg.idleTimeout()
	headersSeen := flattenHeaders(resp.Header)
	idleBody := newIdleReader(resp.Body, streamCancel, steadyIdle, steadyIdle)
	defer idleBody.Close()

	type block struct {
		typ       string
		id        string
		name      string
		args      strings.Builder
		completed bool
	}
	blocks := map[int]*block{}
	var usage Usage
	stop := ""
	sawMessageStop := false

	flushTools := func() error {
		indexes := make([]int, 0)
		for i, b := range blocks {
			if b.typ == "tool_use" && !b.completed {
				indexes = append(indexes, i)
			}
		}
		sort.Ints(indexes)
		for _, i := range indexes {
			b := blocks[i]
			args := strings.TrimSpace(b.args.String())
			if args == "" {
				args = "{}"
			}
			b.completed = true
			if err := emit(ProviderEvent{Kind: "tool_call", ID: b.id, Name: b.name, Args: args}); err != nil {
				return err
			}
		}
		return nil
	}

	err = readSSE(idleBody, func(event string, data []byte) error {
		var env struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			if len(bytes.TrimSpace(data)) <= 2 {
				return nil
			}
			return fmt.Errorf("parse Anthropic stream: %w", err)
		}
		if env.Type == "error" || env.Error.Message != "" {
			msg := "provider error: " + env.Error.Message
			switch env.Error.Type {
			case "overloaded_error":
				return &upstreamError{Status: 529, Body: msg}
			case "api_error":
				return &upstreamError{Status: 500, Body: msg}
			case "rate_limit_error":
				return &upstreamError{Status: 429, Body: msg}
			case "authentication_error", "permission_error":
				return &upstreamError{Status: 401, Body: msg}
			default:
				return fmt.Errorf("%s", msg)
			}
		}
		switch env.Type {
		case "message_start":
			usage.InputTokens = env.Message.Usage.InputTokens
			usage.OutputTokens = env.Message.Usage.OutputTokens
			usage.CacheCreationInputTokens = env.Message.Usage.CacheCreationInputTokens
			usage.CacheReadInputTokens = env.Message.Usage.CacheReadInputTokens
			usage.LatestContextTokens = usage.InputTokens
		case "content_block_start":
			if env.ContentBlock.Type == "tool_use" {
				b := &block{typ: "tool_use", id: env.ContentBlock.ID, name: env.ContentBlock.Name}
				blocks[env.Index] = b
				if err := emit(ProviderEvent{Kind: "tool_start", ID: b.id, Name: b.name}); err != nil {
					return err
				}
			} else {
				blocks[env.Index] = &block{typ: env.ContentBlock.Type}
			}
		case "content_block_delta":
			switch env.Delta.Type {
			case "text_delta":
				if err := emit(ProviderEvent{Kind: "text", Text: env.Delta.Text}); err != nil {
					return err
				}
			case "thinking_delta":
				if err := emit(ProviderEvent{Kind: "thinking", Text: env.Delta.Thinking}); err != nil {
					return err
				}
			case "input_json_delta":
				b := blocks[env.Index]
				if b == nil {
					b = &block{typ: "tool_use"}
					blocks[env.Index] = b
				}
				b.args.WriteString(env.Delta.PartialJSON)
				if err := emit(ProviderEvent{Kind: "tool_input", ID: b.id, Name: b.name, Args: env.Delta.PartialJSON}); err != nil {
					return err
				}
			}
		case "content_block_stop":
			b := blocks[env.Index]
			if b != nil && b.typ == "tool_use" && !b.completed {
				args := strings.TrimSpace(b.args.String())
				if args == "" {
					args = "{}"
				}
				b.completed = true
				if err := emit(ProviderEvent{Kind: "tool_call", ID: b.id, Name: b.name, Args: args}); err != nil {
					return err
				}
			}
		case "message_delta":
			if env.Delta.StopReason != "" {
				stop = mapAnthropicStop(env.Delta.StopReason)
			}
			if env.Usage.OutputTokens > 0 {
				usage.OutputTokens = env.Usage.OutputTokens
			}
		case "message_stop":
			sawMessageStop = true
		}
		return nil
	})
	if err != nil {
		if idleBody.TimedOut() {
			return ProviderResult{}, fmt.Errorf("provider stream was idle for %s", steadyIdle)
		}
		return ProviderResult{}, err
	}
	if err := flushTools(); err != nil {
		return ProviderResult{}, err
	}
	if !sawMessageStop {
		return ProviderResult{}, fmt.Errorf("Anthropic stream ended before message_stop")
	}
	if stop == "" {
		stop = "end_turn"
	}
	hasTools := false
	for _, b := range blocks {
		if b.typ == "tool_use" {
			hasTools = true
			break
		}
	}
	if hasTools {
		stop = "tool_use"
	}
	return ProviderResult{StopReason: stop, Usage: usage, Headers: headersSeen}, nil
}

func mapAnthropicStop(s string) string {
	switch s {
	case "tool_use":
		return "tool_use"
	case "max_tokens":
		return "max_tokens"
	case "refusal":
		return "refusal"
	case "end_turn", "stop_sequence", "":
		return "end_turn"
	default:
		return s
	}
}

func anthropicMessages(msgs []WireMessage, echoReasoning bool) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			content := []map[string]any{}
			if echoReasoning {
				// Anthropic-compatible proxies that reason with unsigned
				// interleaved thinking (DeepSeek/GLM/Kimi) require the prior
				// block to be replayed. Native api.anthropic.com verifies a
				// signature and rejects unsigned blocks, so this is gated on a
				// custom base URL.
				content = append(content, map[string]any{"type": "thinking", "thinking": m.ReasoningContent})
			}
			for _, part := range m.Parts {
				switch part.Type {
				case "text":
					if part.Text != "" {
						content = append(content, map[string]any{"type": "text", "text": part.Text})
					}
				case "tool_use":
					args := part.Args
					if len(args) == 0 || string(args) == "null" {
						args = json.RawMessage(`{}`)
					}
					content = append(content, map[string]any{
						"type":  "tool_use",
						"id":    anthropicWireID(part.ID),
						"name":  part.Name,
						"input": args,
					})
				}
			}
			if len(content) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": content})
			}
		case "user":
			var results, content []map[string]any
			for _, part := range m.Parts {
				switch part.Type {
				case "tool_result":
					txt := part.Content
					if part.IsError && !strings.HasPrefix(strings.TrimSpace(txt), "Error") {
						txt = "Error: " + txt
					}
					resultContent := []map[string]any{{"type": "text", "text": txt}}
					if part.Data != "" && part.Mime != "" {
						resultContent = append(resultContent, map[string]any{
							"type": "image",
							"source": map[string]any{
								"type": "base64", "media_type": part.Mime, "data": part.Data,
							},
						})
					}
					results = append(results, map[string]any{
						"type":        "tool_result",
						"tool_use_id": anthropicWireID(part.ID),
						"content":     resultContent,
						"is_error":    part.IsError,
					})
				case "text":
					if part.Text != "" {
						content = append(content, map[string]any{"type": "text", "text": part.Text})
					}
				case "image":
					content = append(content, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": part.Mime,
							"data":       part.Data,
						},
					})
				case "audio":
					return nil, fmt.Errorf("audio input is not supported by Anthropic models")
				}
			}
			all := append(results, content...)
			if len(all) > 0 {
				out = append(out, map[string]any{"role": "user", "content": all})
			}
		}
	}
	return out, nil
}

func anthropicEchoesUnsignedThinking(baseURL string, model *Model) bool {
	if model == nil || !model.SupportsReasoning {
		return false
	}
	base := strings.ToLower(strings.TrimSpace(baseURL))
	return base != "" && !strings.Contains(base, "api.anthropic.com")
}

func thinkingEnabled(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "off":
		return false
	default:
		return true
	}
}

// parseClaudeVersion reads the major/minor version out of ids like
// claude-sonnet-4-6, claude-4.6-opus or claude-opus-4-20250514.
func parseClaudeVersion(modelID string) (int, int, bool) {
	lower := strings.ToLower(modelID)
	i := strings.Index(lower, "claude")
	if i < 0 {
		return 0, 0, false
	}
	rest := lower[i+len("claude"):]
	re := regexp.MustCompile(`[-/]?(\d+)(?:[-.](\d{1,2}))?(?:$|[^0-9])`)
	m := re.FindStringSubmatch(rest)
	if m == nil {
		return 0, 0, false
	}
	major, _ := strconv.Atoi(m[1])
	minor := 0
	if m[2] != "" {
		minor, _ = strconv.Atoi(m[2])
	}
	return major, minor, true
}

func anthropicUsesAdaptiveThinking(modelID string) bool {
	major, minor, ok := parseClaudeVersion(modelID)
	if !ok {
		return false
	}
	return major > 4 || (major == 4 && minor >= 6)
}

type anthropicOffShape int

const (
	anthropicOffOmit anthropicOffShape = iota
	anthropicOffDisabled
	anthropicOffBetweenTools
	anthropicOffLowEffort
)

func anthropicOffThinking(modelID string) anthropicOffShape {
	major, minor, ok := parseClaudeVersion(modelID)
	if !ok || major < 4 || (major == 4 && minor < 6) {
		return anthropicOffOmit
	}
	if major == 4 {
		return anthropicOffDisabled
	}
	if major == 5 && strings.Contains(strings.ToLower(modelID), "sonnet") {
		if minor == 0 {
			return anthropicOffDisabled
		}
		if minor == 5 {
			return anthropicOffBetweenTools
		}
	}
	return anthropicOffLowEffort
}

func anthropicBudgetFor(maxTokens int, level string) int {
	var cap int
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "low":
		cap = 8192
	case "medium":
		cap = 32768
	case "high":
		cap = minInt(maxTokens, 65536)
	case "xhigh", "max", "ultra":
		cap = maxTokens
	default:
		return 0
	}
	clamped := minInt(cap, maxTokens)
	if clamped >= maxTokens && maxTokens > 1 {
		return maxTokens - 1
	}
	return clamped
}

func anthropicEffort(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max", "ultra":
		return "max"
	default:
		return "low"
	}
}

// anthropicThinkingConfig returns the `thinking` object and optional
// `output_config` for a request. It mirrors ThinkingRuleResolver and
// AnthropicProvider.offThinking in the official app.
func anthropicThinkingConfig(modelID string, supportsReasoning bool, level string, maxTokens int) (map[string]any, map[string]any) {
	if thinkingEnabled(level) && supportsReasoning {
		if anthropicUsesAdaptiveThinking(modelID) {
			return map[string]any{"type": "adaptive", "display": "summarized"},
				map[string]any{"effort": anthropicEffort(level)}
		}
		budget := anthropicBudgetFor(maxTokens, level)
		if budget > 0 {
			return map[string]any{"type": "enabled", "budget_tokens": budget}, nil
		}
		return nil, nil
	}
	switch anthropicOffThinking(modelID) {
	case anthropicOffDisabled:
		return map[string]any{"type": "disabled"}, nil
	case anthropicOffBetweenTools:
		return map[string]any{"type": "between_tools"}, nil
	case anthropicOffLowEffort:
		return map[string]any{"type": "adaptive", "display": "summarized"},
			map[string]any{"effort": "low"}
	default:
		return nil, nil
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var anthropicIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// anthropicWireID maps an id from another provider onto Anthropic's accepted
// id shape. Valid ids are kept; ids containing separators such as the
// Responses API's `call_…|fc_…` become a stable hash so the matching
// tool_result still points at the same block.
func anthropicWireID(id string) string {
	if anthropicIDPattern.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "toolu_" + hex.EncodeToString(sum[:16])
}
