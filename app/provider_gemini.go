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
	body := map[string]any{
		"contents": contents,
		"generationConfig": map[string]any{
			"maxOutputTokens": effectiveMaxTokens(req.Model, req.MaxTokens),
		},
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
	reqURL := base + "/models/" + url.PathEscape(req.Model.ID) + ":streamGenerateContent?alt=sse&key=" + url.QueryEscape(p.cfg.APIKey)
	headers := map[string]string{}
	for k, v := range p.cfg.Headers {
		headers[sanitizeProviderHeaderValue(k)] = sanitizeProviderHeaderValue(v)
	}
	resp, err := p.http.doJSON(ctx, "POST", reqURL, headers, body, true)
	if err != nil {
		return ProviderResult{}, err
	}
	defer resp.Body.Close()

	var usage Usage
	var finish string
	toolIndex := 0
	err = readSSE(resp.Body, func(event string, data []byte) error {
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
			return fmt.Errorf("Gemini error: %s", chunk.Error.Message)
		}
		if chunk.UsageMetadata != nil {
			u := chunk.UsageMetadata
			usage = Usage{
				InputTokens: u.PromptTokenCount, OutputTokens: u.CandidatesTokenCount,
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
			case "image":
				parts = append(parts, map[string]any{"inlineData": map[string]any{
					"mimeType": p.Mime, "data": p.Data,
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
			}
		}
		if len(parts) > 0 {
			out = append(out, map[string]any{"role": role, "parts": parts})
		}
	}
	return out, nil
}
