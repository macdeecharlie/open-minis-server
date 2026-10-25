package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type anthropicProvider struct {
	cfg  *Provider
	http *providerHTTP
}

func (p *anthropicProvider) Stream(ctx context.Context, req ProviderRequest, emit func(ProviderEvent) error) (ProviderResult, error) {
	msgs, err := anthropicMessages(req.Messages)
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
	if budget := anthropicThinkingBudget(req.Thinking); budget > 0 {
		if maxTokens <= budget+1024 {
			maxTokens = budget + 4096
			body["max_tokens"] = maxTokens
		}
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	if strings.TrimSpace(req.System) != "" {
		body["system"] = req.System
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Function.Name,
				"description":  t.Function.Description,
				"input_schema": t.Function.Parameters,
			})
		}
		body["tools"] = tools
	}
	headers := map[string]string{
		"x-api-key":         p.cfg.APIKey,
		"anthropic-version": "2023-06-01",
	}
	for k, v := range p.cfg.Headers {
		headers[sanitizeProviderHeaderValue(k)] = sanitizeProviderHeaderValue(v)
	}
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	resp, err := p.http.doJSON(streamCtx, "POST", anthropicEndpoint(p.cfg.BaseURL), headers, body, true)
	if err != nil {
		return ProviderResult{}, err
	}
	idleBody := newIdleReader(resp.Body, streamCancel, providerIdleTimeout)
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
			return fmt.Errorf("provider error: %s", env.Error.Message)
		}
		switch env.Type {
		case "message_start":
			usage.InputTokens = env.Message.Usage.InputTokens
			usage.OutputTokens = env.Message.Usage.OutputTokens
			usage.CacheCreationInputTokens = env.Message.Usage.CacheCreationInputTokens
			usage.CacheReadInputTokens = env.Message.Usage.CacheReadInputTokens
			usage.LatestContextTokens = usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
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
			return ProviderResult{}, fmt.Errorf("provider stream was idle for %s", providerIdleTimeout)
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
	return ProviderResult{StopReason: stop, Usage: usage}, nil
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

func anthropicMessages(msgs []WireMessage) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			content := []map[string]any{}
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
						"id":    part.ID,
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
					results = append(results, map[string]any{
						"type":        "tool_result",
						"tool_use_id": part.ID,
						"content":     txt,
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

func anthropicThinkingBudget(thinking string) int {
	switch strings.ToLower(strings.TrimSpace(thinking)) {
	case "low":
		return 1024
	case "medium":
		return 2048
	case "high":
		return 4096
	case "xhigh":
		return 8192
	case "max":
		return 16384
	case "ultra":
		return 24576
	default:
		return 0
	}
}
