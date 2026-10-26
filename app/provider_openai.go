package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type openAIProvider struct {
	cfg  *Provider
	http *providerHTTP
}

func (p *openAIProvider) Stream(ctx context.Context, req ProviderRequest, emit func(ProviderEvent) error) (ProviderResult, error) {
	if shouldUseResponsesAPI(p.cfg.BaseURL, req.Model.ID) {
		return (&responsesProvider{cfg: p.cfg, http: p.http}).Stream(ctx, req, emit)
	}
	msgs, err := openAIMessages(req.Messages, modelSupportsImages(req.Model), suppressReasoningEcho(p.cfg.BaseURL, req.Model.ID))
	if err != nil {
		return ProviderResult{}, err
	}
	if strings.TrimSpace(req.System) != "" {
		msgs = append([]map[string]any{{"role": "system", "content": req.System}}, msgs...)
	}
	body := map[string]any{
		"model":    req.Model.ID,
		"messages": msgs,
		"stream":   true,
	}
	if openAIUsesCompletionTokens(p.cfg.BaseURL, req.Model.ID) {
		body["max_completion_tokens"] = effectiveMaxTokens(req.Model, req.MaxTokens)
	} else {
		body["max_tokens"] = effectiveMaxTokens(req.Model, req.MaxTokens)
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
		body["tool_choice"] = "auto"
	}
	if effort := openAIReasoningEffort(req.Model, req.Thinking); effort != "" {
		body["reasoning_effort"] = effort
	}
	body["stream_options"] = map[string]any{"include_usage": true}

	headers := map[string]string{}
	if p.cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + p.cfg.APIKey
	}
	for k, v := range p.cfg.Headers {
		headers[sanitizeProviderHeaderValue(k)] = sanitizeProviderHeaderValue(v)
	}
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	resp, err := p.http.doJSON(streamCtx, "POST", openAIEndpoint(p.cfg.BaseURL), headers, body, true)
	if err != nil {
		return ProviderResult{}, err
	}
	idleBody := newIdleReader(resp.Body, streamCancel, providerIdleTimeout)
	defer idleBody.Close()

	type callState struct {
		id        string
		name      string
		args      strings.Builder
		started   bool
		completed bool
	}
	calls := map[int]*callState{}
	finish := ""
	var usage Usage
	sawDone := false
	var textSeen bool

	flushCalls := func() error {
		indexes := make([]int, 0, len(calls))
		for idx := range calls {
			indexes = append(indexes, idx)
		}
		sort.Ints(indexes)
		for _, idx := range indexes {
			c := calls[idx]
			if c.completed || c.name == "" {
				continue
			}
			c.completed = true
			args := strings.TrimSpace(c.args.String())
			if args == "" {
				args = "{}"
			}
			if err := emit(ProviderEvent{Kind: "tool_call", ID: c.id, Name: c.name, Args: args}); err != nil {
				return err
			}
		}
		return nil
	}

	err = readSSE(idleBody, func(event string, data []byte) error {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			sawDone = true
			return nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				CompletionTokensDetails struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			// Some OpenAI-compatible relays send a bare keepalive.
			if len(bytes.TrimSpace(data)) <= 2 {
				return nil
			}
			return fmt.Errorf("parse provider stream: %w", err)
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return fmt.Errorf("provider error: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			usage.InputTokens = chunk.Usage.PromptTokens
			usage.OutputTokens = chunk.Usage.CompletionTokens
			usage.CacheReadInputTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			usage.LatestContextTokens = chunk.Usage.PromptTokens
			usage.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
		}
		for _, choice := range chunk.Choices {
			d := choice.Delta
			if d.Content != "" {
				textSeen = true
				if err := emit(ProviderEvent{Kind: "text", Text: d.Content}); err != nil {
					return err
				}
			}
			reasoning := d.ReasoningContent
			if reasoning == "" {
				reasoning = d.Reasoning
			}
			if reasoning != "" {
				if err := emit(ProviderEvent{Kind: "thinking", Text: reasoning}); err != nil {
					return err
				}
			}
			for _, tc := range d.ToolCalls {
				c := calls[tc.Index]
				if c == nil {
					c = &callState{}
					calls[tc.Index] = c
				}
				if tc.ID != "" {
					c.id = tc.ID
				}
				if tc.Function.Name != "" {
					c.name = tc.Function.Name
				}
				if c.id == "" {
					c.id = fmt.Sprintf("call_%d", tc.Index)
				}
				if !c.started && c.name != "" {
					c.started = true
					if err := emit(ProviderEvent{Kind: "tool_start", ID: c.id, Name: c.name}); err != nil {
						return err
					}
				}
				if tc.Function.Arguments != "" {
					c.args.WriteString(tc.Function.Arguments)
					if err := emit(ProviderEvent{Kind: "tool_input", ID: c.id, Name: c.name, Args: tc.Function.Arguments}); err != nil {
						return err
					}
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		if idleBody.TimedOut() {
			return ProviderResult{}, fmt.Errorf("provider stream was idle for %s", providerIdleTimeout)
		}
		return ProviderResult{}, err
	}
	if err := flushCalls(); err != nil {
		return ProviderResult{}, err
	}
	if usage.OutputTokens == 0 && usage.InputTokens == 0 {
		// Some relays only put usage in a final non-stream chunk.
	}
	stop := mapOpenAIStop(finish)
	if len(calls) > 0 {
		stop = "tool_use"
	}
	if finish == "" && !sawDone && !textSeen && len(calls) == 0 {
		return ProviderResult{}, fmt.Errorf("provider stream ended without a finish reason")
	}
	if stop == "" {
		stop = "end_turn"
	}
	return ProviderResult{StopReason: stop, Usage: usage}, nil
}

func mapOpenAIStop(s string) string {
	switch s {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length", "max_tokens":
		return "max_tokens"
	case "content_filter", "refusal":
		return "refusal"
	case "stop", "end_turn", "":
		return "end_turn"
	default:
		return s
	}
}

func openAIMessages(msgs []WireMessage, supportsImages, suppressReasoning bool) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			text, _ := wireTextAndImages(m)
			var toolCalls []map[string]any
			for _, part := range m.Parts {
				if part.Type != "tool_use" {
					continue
				}
				args := strings.TrimSpace(string(part.Args))
				if args == "" || args == "null" {
					args = "{}"
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   part.ID,
					"type": "function",
					"function": map[string]any{
						"name":      part.Name,
						"arguments": args,
					},
				})
			}
			msg := map[string]any{"role": "assistant", "content": text}
			if len(toolCalls) > 0 {
				msg["tool_calls"] = toolCalls
			}
			if text == "" && len(toolCalls) == 0 {
				continue
			}
			if strings.TrimSpace(m.ReasoningContent) != "" && !suppressReasoning {
				msg["reasoning_content"] = m.ReasoningContent
			}
			out = append(out, msg)
		case "user":
			var texts []string
			var images []WirePart
			var audios []WirePart
			var results []WirePart
			for _, part := range m.Parts {
				switch part.Type {
				case "text":
					texts = append(texts, part.Text)
				case "image":
					images = append(images, part)
				case "audio":
					audios = append(audios, part)
				case "tool_result":
					results = append(results, part)
				}
			}
			var carriers []map[string]any
			for _, r := range results {
				content := r.Content
				if r.IsError && !strings.HasPrefix(strings.TrimSpace(content), "Error") {
					content = "Error: " + content
				}
				out = append(out, map[string]any{
					"role":         "tool",
					"tool_call_id": r.ID,
					"content":      content,
				})
				if supportsImages && r.Data != "" && r.Mime != "" {
					carriers = append(carriers, map[string]any{
						"role": "user",
						"content": []map[string]any{
							{"type": "text", "text": "[Image returned by " + r.Name + " (tool_call_id: " + r.ID + ")]"},
							{"type": "image_url", "image_url": map[string]any{
								"url": "data:" + r.Mime + ";base64," + r.Data,
							}},
						},
					})
				}
			}
			out = append(out, carriers...)
			if len(texts) > 0 || len(images) > 0 || len(audios) > 0 {
				content := strings.Join(texts, "")
				if len(images) > 0 || len(audios) > 0 {
					parts := []map[string]any{}
					if content != "" {
						parts = append(parts, map[string]any{"type": "text", "text": content})
					}
					for _, img := range images {
						parts = append(parts, map[string]any{
							"type": "image_url",
							"image_url": map[string]any{
								"url": "data:" + img.Mime + ";base64," + img.Data,
							},
						})
					}
					for _, a := range audios {
						format := a.Format
						if format == "" {
							format = strings.TrimPrefix(a.Mime, "audio/")
						}
						parts = append(parts, map[string]any{
							"type": "input_audio",
							"input_audio": map[string]any{
								"data": a.Data, "format": format,
							},
						})
					}
					out = append(out, map[string]any{"role": "user", "content": parts})
				} else {
					out = append(out, map[string]any{"role": "user", "content": content})
				}
			}
		default:
			// Ignore unknown roles rather than sending a payload every provider
			// would reject. The client only sends user/assistant.
		}
	}
	return out, nil
}

func openAIUsesCompletionTokens(baseURL, modelID string) bool {
	base := strings.ToLower(baseURL)
	if strings.Contains(base, "api.openai.com") {
		return true
	}
	id := strings.ToLower(modelID)
	return strings.HasPrefix(id, "gpt-5") || strings.HasPrefix(id, "o3") || strings.HasPrefix(id, "o4")
}

// OpenAI accepts minimal/low/medium/high. The app's richer levels (xhigh,
// max, ultra) are UI intent, not API values; clamp them rather than sending a
// value the provider rejects.
func openAIReasoningEffort(model *Model, thinking string) string {
	if !model.SupportsReasoning {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(thinking)) {
	case "", "off":
		return ""
	case "minimal", "low", "medium", "high":
		return strings.ToLower(thinking)
	case "xhigh", "max", "ultra":
		return "high"
	default:
		return "medium"
	}
}

func modelSupportsImages(model *Model) bool {
	for _, m := range model.InputModalities {
		if strings.EqualFold(m, "image") {
			return true
		}
	}
	return false
}

// Some strict OpenAI-compatible vendors reject an unknown reasoning_content
// field on historical assistant turns. The official client suppresses the echo
// for these families; keep the same behavior instead of breaking turn two.
func suppressReasoningEcho(baseURL, modelID string) bool {
	hay := strings.ToLower(baseURL + " " + modelID)
	return strings.Contains(hay, "mistral") || strings.Contains(hay, "cerebras")
}

// Direct first-party OpenAI models that are native Responses users. Relays
// such as OpenRouter keep Chat Completions; the official app makes the same
// split.
func shouldUseResponsesAPI(baseURL, modelID string) bool {
	base := strings.ToLower(baseURL)
	if !strings.Contains(base, "api.openai.com") {
		return false
	}
	id := strings.ToLower(modelID)
	for _, prefix := range []string{"gpt-5", "gpt-6", "o3", "o4", "codex"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}
