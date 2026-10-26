package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// responsesProvider implements OpenAI's Responses API. It is the native path
// for models that use /v1/responses (newer first-party OpenAI models and
// Codex-style endpoints); Chat Completions remains the default for
// OpenAI-compatible relays.
type responsesProvider struct {
	cfg  *Provider
	http *providerHTTP
}

func responsesRequestURL(cfg *Provider, modelID string) string {
	if cfg.Azure {
		base, query := splitQuery(cfg.BaseURL)
		return strings.TrimSuffix(base, "/") + "/openai/deployments/" + url.PathEscape(modelID) + "/responses" + query
	}
	return responsesEndpoint(cfg.BaseURL)
}

func responsesEndpoint(base string) string {
	b := strings.TrimSuffix(strings.TrimSpace(base), "/")
	if strings.HasSuffix(b, "/responses") {
		return b
	}
	if strings.HasSuffix(b, "/v1") {
		return b + "/responses"
	}
	return b + "/v1/responses"
}

func responsesUsage(input, output, cached, reasoning int) Usage {
	// OpenAI's input_tokens includes cached input. The app's Usage model keeps
	// them separate and uses the total for context pressure.
	nonCached := input - cached
	if nonCached < 0 {
		nonCached = 0
	}
	return Usage{
		InputTokens: nonCached, OutputTokens: output,
		CacheReadInputTokens: cached, ReasoningTokens: reasoning,
		LatestContextTokens: input,
	}
}

func (p *responsesProvider) Stream(ctx context.Context, req ProviderRequest, emit func(ProviderEvent) error) (ProviderResult, error) {
	input, err := responsesInput(req.Messages, modelSupportsImages(req.Model))
	if err != nil {
		return ProviderResult{}, err
	}
	body := map[string]any{
		"model":               req.Model.ID,
		"input":               input,
		"stream":              true,
		"store":               false,
		"parallel_tool_calls": true,
		"max_output_tokens":   effectiveMaxTokens(req.Model, req.MaxTokens),
	}
	if strings.TrimSpace(req.System) != "" {
		body["instructions"] = req.System
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function", "name": t.Function.Name,
				"description": t.Function.Description,
				"parameters":  t.Function.Parameters,
			})
		}
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	if effort := openAIReasoningEffortFor(p.cfg.BaseURL, req.Model, req.Thinking); effort != "" {
		reasoning := map[string]any{"effort": effort}
		if thinkingEnabled(req.Thinking) {
			reasoning["summary"] = "auto"
		}
		body["reasoning"] = reasoning
	}
	if key := responsesPromptCacheKey(req.Messages); key != "" {
		body["prompt_cache_key"] = key
	}
	headers := map[string]string{}
	if p.cfg.APIKey != "" {
		if p.cfg.Azure {
			headers["api-key"] = p.cfg.APIKey
		} else {
			headers["Authorization"] = "Bearer " + p.cfg.APIKey
		}
	}
	for k, v := range p.cfg.Headers {
		headers[sanitizeProviderHeaderValue(k)] = sanitizeProviderHeaderValue(v)
	}
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	resp, err := p.http.doJSON(streamCtx, "POST", responsesRequestURL(p.cfg, req.Model.ID), headers, body, true)
	if err != nil {
		return ProviderResult{}, err
	}
	firstIdle, steadyIdle := openAIIdle(p.cfg, req.IdleTimeout)
	idleBody := newIdleReader(resp.Body, streamCancel, firstIdle, steadyIdle)
	defer idleBody.Close()

	type callState struct {
		id   string
		name string
		args strings.Builder
	}
	calls := map[string]*callState{}
	toolOrder := []string{}
	var usage Usage
	var stop string
	sawTerminal := false

	err = readSSE(idleBody, func(event string, data []byte) error {
		var ev struct {
			Type   string `json:"type"`
			Delta  string `json:"delta"`
			ItemID string `json:"item_id"`
			Item   struct {
				ID        string `json:"id"`
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response struct {
				Status string `json:"status"`
				Usage  struct {
					InputTokens        int `json:"input_tokens"`
					OutputTokens       int `json:"output_tokens"`
					InputTokensDetails struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
					OutputTokensDetails struct {
						ReasoningTokens int `json:"reasoning_tokens"`
					} `json:"output_tokens_details"`
				} `json:"usage"`
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
				IncompleteDetails *struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			} `json:"response"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if strings.TrimSpace(string(data)) == "[DONE]" {
			return nil
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			if len(strings.TrimSpace(string(data))) <= 2 {
				return nil
			}
			return fmt.Errorf("parse Responses stream: %w", err)
		}
		switch ev.Type {
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			if ev.Delta != "" {
				return emit(ProviderEvent{Kind: "thinking", Text: ev.Delta})
			}
		case "response.output_text.delta":
			if ev.Delta != "" {
				return emit(ProviderEvent{Kind: "text", Text: ev.Delta})
			}
		case "response.output_item.added":
			if ev.Item.Type == "function_call" && ev.Item.CallID != "" && ev.Item.Name != "" {
				calls[ev.Item.ID] = &callState{id: ev.Item.CallID, name: ev.Item.Name}
				toolOrder = append(toolOrder, ev.Item.ID)
				return emit(ProviderEvent{Kind: "tool_start", ID: ev.Item.CallID, Name: ev.Item.Name})
			}
		case "response.function_call_arguments.delta":
			if c := calls[ev.ItemID]; c != nil && ev.Delta != "" {
				c.args.WriteString(ev.Delta)
				return emit(ProviderEvent{Kind: "tool_input", ID: c.id, Name: c.name, Args: ev.Delta})
			}
		case "response.output_item.done":
			if ev.Item.Type != "function_call" {
				return nil
			}
			c := calls[ev.Item.ID]
			if c == nil {
				return nil
			}
			args := strings.TrimSpace(ev.Item.Arguments)
			if args == "" {
				args = strings.TrimSpace(c.args.String())
			}
			if args == "" {
				args = "{}"
			}
			delete(calls, ev.Item.ID)
			return emit(ProviderEvent{Kind: "tool_call", ID: c.id, Name: c.name, Args: args})
		case "response.completed":
			sawTerminal = true
			u := ev.Response.Usage
			usage = responsesUsage(u.InputTokens, u.OutputTokens, u.InputTokensDetails.CachedTokens, u.OutputTokensDetails.ReasoningTokens)
			stop = "end_turn"
			return nil
		case "response.incomplete":
			sawTerminal = true
			u := ev.Response.Usage
			usage = responsesUsage(u.InputTokens, u.OutputTokens, u.InputTokensDetails.CachedTokens, u.OutputTokensDetails.ReasoningTokens)
			reason := ""
			if ev.Response.IncompleteDetails != nil {
				reason = ev.Response.IncompleteDetails.Reason
			}
			switch reason {
			case "max_output_tokens":
				stop = "max_tokens"
			case "content_filter":
				stop = "refusal"
			default:
				stop = "end_turn"
			}
			return nil
		case "response.failed":
			sawTerminal = true
			msg := "response.failed"
			code := ""
			if ev.Response.Error != nil {
				code = ev.Response.Error.Code
				if ev.Response.Error.Message != "" {
					msg = ev.Response.Error.Message
				}
				if code != "" {
					msg = "[" + code + "] " + msg
				}
			}
			status := 0
			switch code {
			case "server_error":
				status = 500
			case "rate_limit_exceeded":
				status = 429
			case "context_length_exceeded":
				status = 400
			}
			if status > 0 {
				return &upstreamError{Status: status, Body: msg}
			}
			return fmt.Errorf("%s", msg)
		case "error":
			if ev.Error != nil && ev.Error.Message != "" {
				return fmt.Errorf("%s", ev.Error.Message)
			}
		}
		return nil
	})
	if err != nil {
		if idleBody.TimedOut() {
			return ProviderResult{}, fmt.Errorf("provider stream was idle for %s", steadyIdle)
		}
		return ProviderResult{}, err
	}
	if !sawTerminal {
		return ProviderResult{}, fmt.Errorf("Responses stream ended without a terminal event")
	}
	// Flush any call that never got an output_item.done.
	for _, itemID := range toolOrder {
		c := calls[itemID]
		if c == nil {
			continue
		}
		args := strings.TrimSpace(c.args.String())
		if args == "" {
			args = "{}"
		}
		if err := emit(ProviderEvent{Kind: "tool_call", ID: c.id, Name: c.name, Args: args}); err != nil {
			return ProviderResult{}, err
		}
		delete(calls, itemID)
	}
	if len(toolOrder) > 0 {
		stop = "tool_use"
	}
	if stop == "" {
		stop = "end_turn"
	}
	return ProviderResult{StopReason: stop, Usage: usage}, nil
}

func responsesInput(messages []WireMessage, supportsImages bool) ([]map[string]any, error) {
	input := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case "assistant":
			var text strings.Builder
			var functionCalls []map[string]any
			for _, p := range m.Parts {
				switch p.Type {
				case "text":
					text.WriteString(p.Text)
				case "tool_use":
					args := strings.TrimSpace(string(p.Args))
					if args == "" || args == "null" {
						args = "{}"
					}
					callID := capResponsesID(p.ID, "call")
					fcID := capResponsesID("fc_"+p.ID, "fc")
					functionCalls = append(functionCalls, map[string]any{
						"type": "function_call", "id": fcID, "call_id": callID,
						"name": p.Name, "arguments": args,
					})
				}
			}
			if text.Len() > 0 {
				input = append(input, map[string]any{"role": "assistant", "content": text.String()})
			}
			input = append(input, functionCalls...)
		case "user":
			var outputs []map[string]any
			var content []map[string]any
			var carriers []map[string]any
			for _, p := range m.Parts {
				switch p.Type {
				case "tool_result":
					outputs = append(outputs, map[string]any{
						"type": "function_call_output", "call_id": capResponsesID(p.ID, "call"),
						"output": p.Content,
					})
					if supportsImages && p.Data != "" && p.Mime != "" {
						carriers = append(carriers, map[string]any{
							"role": "user",
							"content": []map[string]any{
								{"type": "input_text", "text": "[Image returned by " + p.Name + "]"},
								{"type": "input_image", "image_url": "data:" + p.Mime + ";base64," + p.Data},
							},
						})
					}
				case "text":
					if p.Text != "" {
						content = append(content, map[string]any{"type": "input_text", "text": p.Text})
					}
				case "image":
					if supportsImages && p.Data != "" {
						content = append(content, map[string]any{
							"type": "input_image", "image_url": "data:" + p.Mime + ";base64," + p.Data,
						})
					} else if !supportsImages {
						content = append(content, map[string]any{"type": "input_text", "text": "[Image attached but this model does not support vision input]"})
					}
				case "audio":
					content = append(content, map[string]any{"type": "input_text", "text": "[Audio attachment omitted: the Responses text path does not accept audio]"})
				}
			}
			input = append(input, outputs...)
			if len(content) > 0 {
				input = append(input, map[string]any{"role": "user", "content": content})
			}
			input = append(input, carriers...)
		}
	}
	return input, nil
}

func responsesPromptCacheKey(messages []WireMessage) string {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				sum := sha256.Sum256([]byte(p.Text))
				return hex.EncodeToString(sum[:16])
			}
		}
	}
	return ""
}

func capResponsesID(id, prefix string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		id = newID(prefix)
	}
	var b strings.Builder
	b.Grow(len(id) + len(prefix) + 1)
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	id = b.String()
	if !strings.HasPrefix(id, prefix+"_") {
		id = prefix + "_" + id
	}
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}
