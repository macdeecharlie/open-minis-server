package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// geminiProvider talks to Google's native generateContent streaming API. It is
// separate from the OpenAI-compatible provider because Gemini's tool-call and
// message shapes are genuinely different (no tool ids, functionResponse by
// name, systemInstruction).
type geminiProvider struct {
	cfg  *Provider
	http *providerHTTP
}

func (p *geminiProvider) Stream(ctx context.Context, req ProviderRequest, emit func(ProviderEvent) error) (ProviderResult, error) {
	contents, err := geminiContents(req.Messages)
	if err != nil {
		return ProviderResult{}, err
	}
	generation := map[string]any{
		"maxOutputTokens": effectiveMaxTokens(req.Model, req.MaxTokens),
	}
	if cfg := geminiThinkingConfig(req.Model.ID, req.Thinking); cfg != nil {
		generation["thinkingConfig"] = cfg
	}
	body := map[string]any{
		"contents":         contents,
		"generationConfig": generation,
	}
	if strings.TrimSpace(req.System) != "" {
		body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": req.System}}}
	}
	if len(req.Tools) > 0 {
		decls := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, map[string]any{
				"name":        t.Function.Name,
				"description": t.Function.Description,
				"parameters":  t.Function.Parameters,
			})
		}
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	base := strings.TrimSuffix(p.cfg.BaseURL, "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com/v1beta"
	}
	reqURL := base + "/models/" + url.PathEscape(req.Model.ID) + ":streamGenerateContent?alt=sse"
	headers := map[string]string{"x-goog-api-key": p.cfg.APIKey}
	for k, v := range p.cfg.Headers {
		headers[sanitizeProviderHeaderValue(k)] = sanitizeProviderHeaderValue(v)
	}
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	resp, err := p.http.doJSON(streamCtx, "POST", reqURL, headers, body, true)
	if err != nil {
		return ProviderResult{}, err
	}
	steadyIdle := p.cfg.idleTimeout()
	idleBody := newIdleReader(resp.Body, streamCancel, steadyIdle, steadyIdle)
	defer idleBody.Close()

	var usage Usage
	var finish string
	toolIndex := 0
	err = readSSE(idleBody, func(event string, data []byte) error {
		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text             string `json:"text"`
						Thought          bool   `json:"thought"`
						ThoughtSignature string `json:"thoughtSignature"`
						FunctionCall     *struct {
							Name string          `json:"name"`
							Args json.RawMessage `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount        int `json:"promptTokenCount"`
				CandidatesTokenCount    int `json:"candidatesTokenCount"`
				CachedContentTokenCount int `json:"cachedContentTokenCount"`
				ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
			} `json:"usageMetadata"`
			Error *struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			if len(strings.TrimSpace(string(data))) <= 2 {
				return nil
			}
			return fmt.Errorf("parse Gemini stream: %w", err)
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			msg := "Gemini error: " + chunk.Error.Message
			switch chunk.Error.Status {
			case "UNAVAILABLE", "INTERNAL":
				return &upstreamError{Status: 503, Body: msg}
			case "RESOURCE_EXHAUSTED":
				return &upstreamError{Status: 429, Body: msg}
			case "INVALID_ARGUMENT", "PERMISSION_DENIED", "UNAUTHENTICATED":
				return &upstreamError{Status: 400, Body: msg}
			default:
				return fmt.Errorf("%s", msg)
			}
		}
		if chunk.UsageMetadata != nil {
			u := chunk.UsageMetadata
			input := u.PromptTokenCount - u.CachedContentTokenCount
			if input < 0 {
				input = 0
			}
			usage = Usage{
				InputTokens: input, OutputTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount,
				CacheReadInputTokens: u.CachedContentTokenCount,
				ReasoningTokens:      u.ThoughtsTokenCount,
				LatestContextTokens:  u.PromptTokenCount,
			}
		}
		for _, c := range chunk.Candidates {
			for _, part := range c.Content.Parts {
				if part.FunctionCall != nil && part.FunctionCall.Name != "" {
					toolIndex++
					id := fmt.Sprintf("call_gemini_%d", toolIndex)
					if err := emit(ProviderEvent{Kind: "tool_start", ID: id, Name: part.FunctionCall.Name}); err != nil {
						return err
					}
					args := strings.TrimSpace(string(part.FunctionCall.Args))
					if args == "" || args == "null" {
						args = "{}"
					}
					if err := emit(ProviderEvent{Kind: "tool_call", ID: id, Name: part.FunctionCall.Name, Args: args, Signature: part.ThoughtSignature}); err != nil {
						return err
					}
					continue
				}
				if part.Text != "" {
					kind := "text"
					if part.Thought {
						kind = "thinking"
					}
					if err := emit(ProviderEvent{Kind: kind, Text: part.Text}); err != nil {
						return err
					}
				}
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
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
	stop := mapGeminiStop(finish)
	if toolIndex > 0 {
		stop = "tool_use"
	}
	if stop == "" {
		stop = "end_turn"
	}
	if usage.LatestContextTokens == 0 {
		usage.LatestContextTokens = usage.InputTokens + usage.CacheReadInputTokens
	}
	return ProviderResult{StopReason: stop, Usage: usage}, nil
}

func mapGeminiStop(s string) string {
	switch s {
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "refusal"
	case "MALFORMED_FUNCTION_CALL":
		return "error"
	case "STOP", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func geminiContents(msgs []WireMessage) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		parts := make([]map[string]any, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				if p.Text != "" {
					parts = append(parts, map[string]any{"text": p.Text})
				}
			case "image", "audio":
				mime := p.Mime
				if p.Type == "audio" && mime == "" {
					mime = "audio/" + p.Format
				}
				parts = append(parts, map[string]any{"inlineData": map[string]any{
					"mimeType": mime, "data": p.Data,
				}})
			case "tool_use":
				args := p.Args
				if len(args) == 0 || string(args) == "null" {
					args = json.RawMessage(`{}`)
				}
				var obj any
				_ = json.Unmarshal(args, &obj)
				part := map[string]any{"functionCall": map[string]any{"name": p.Name, "args": obj}}
				if p.ThoughtSignature != "" {
					part["thoughtSignature"] = p.ThoughtSignature
				}
				parts = append(parts, part)
			case "tool_result":
				parts = append(parts, map[string]any{"functionResponse": map[string]any{
					"name": p.Name,
					"response": map[string]any{
						"result": p.Content,
						"error":  p.IsError,
					},
				}})
				if p.Data != "" {
					mime := p.Mime
					if mime == "" {
						mime = "image/png"
					}
					parts = append(parts, map[string]any{"text": "[Image returned by " + p.Name + "]"})
					parts = append(parts, map[string]any{"inlineData": map[string]any{
						"mimeType": mime, "data": p.Data,
					}})
				}
			}
		}
		if len(parts) > 0 {
			out = append(out, map[string]any{"role": role, "parts": parts})
		}
	}
	return out, nil
}

// geminiThinkingConfig mirrors the model-family rules in the official
// ThinkingRuleResolver: Gemini 3.x takes a thinkingLevel string, 2.5 Pro/Flash
// take a token budget, and Flash Lite has no thinking control. Only enabled
// levels are sent; omitting the field preserves each model's default.
func geminiThinkingConfig(modelID, thinking string) map[string]any {
	id := strings.ToLower(modelID)
	level := strings.ToLower(strings.TrimSpace(thinking))
	if level == "" || level == "off" {
		return nil
	}
	if strings.Contains(id, "gemini-3") {
		switch level {
		case "minimal", "low":
			level = "low"
		case "medium":
			level = "medium"
		case "high", "xhigh", "max", "ultra":
			level = "high"
		default:
			level = "medium"
		}
		return map[string]any{"thinkingLevel": level, "includeThoughts": true}
	}
	if strings.Contains(id, "flash-lite") {
		return nil
	}
	var budget int
	switch {
	case strings.Contains(id, "2.5-pro"):
		budget = map[string]int{"low": 1024, "medium": 4096, "high": 8192, "xhigh": 16384, "max": 16384, "ultra": 16384}[level]
	case strings.Contains(id, "2.5-flash"):
		budget = map[string]int{"low": 1024, "medium": 4096, "high": 8192, "xhigh": 8192, "max": 8192, "ultra": 8192}[level]
	default:
		return nil
	}
	if budget == 0 {
		return nil
	}
	return map[string]any{"thinkingBudget": budget, "includeThoughts": true}
}
