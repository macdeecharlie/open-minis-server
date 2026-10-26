package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// handleChatCompletions is an OpenAI-compatible pass-through used by the
// Android client's auxiliary calls (title generation, compaction, quick
// tests). The agent loop itself uses /v1/agent/runs. Provider credentials
// never leave the server: the client's bearer token is replaced here.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var meta struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	provider, _, err := s.cfg.ResolveModel(meta.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if provider.Kind == "anthropic" || provider.Kind == "gemini" || provider.Kind == "openai-responses" {
		s.handleAuxiliaryOpenAI(w, r, provider, meta.Model, body)
		return
	}
	if provider.Kind == "mock" {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion",
			"created": nowMillis() / 1000,
			"model":   meta.Model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "OpenMinis mock provider response."},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11},
		})
		return
	}

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, openAIEndpoint(provider.BaseURL), bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	copyProxyHeaders(upReq.Header, r.Header)
	upReq.Header.Set("Content-Type", "application/json")
	if provider.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}
	for k, v := range provider.Headers {
		upReq.Header.Set(sanitizeProviderHeaderValue(k), sanitizeProviderHeaderValue(v))
	}
	upReq.Header.Set("User-Agent", "open-minis-server/"+serverVersion)

	resp, err := s.http.client.Do(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	copyProxyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				log.Printf("chat proxy read: %v", rerr)
			}
			return
		}
	}
}

func copyProxyHeaders(dst, src http.Header) {
	for k, vals := range src {
		if isHopByHopHeader(k) || strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Host") || strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func isHopByHopHeader(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

type openAIChatRequest struct {
	Model         string            `json:"model"`
	Messages      []json.RawMessage `json:"messages"`
	Tools         []ToolDef         `json:"tools"`
	MaxTokens     int               `json:"max_tokens"`
	MaxCompletion int               `json:"max_completion_tokens"`
	Stream        bool              `json:"stream"`
}

type openAIChatMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
}

func openAIWireMessages(raw []json.RawMessage) (system string, out []WireMessage) {
	for _, r := range raw {
		var m openAIChatMessage
		if err := json.Unmarshal(r, &m); err != nil {
			continue
		}
		switch m.Role {
		case "system", "developer":
			system += openAIContentText(m.Content)
		case "tool":
			out = append(out, WireMessage{Role: "user", Parts: []WirePart{{
				Type: "tool_result", ID: m.ToolCallID, Name: m.Name,
				Content: openAIContentText(m.Content),
			}}})
		default:
			msg := WireMessage{Role: m.Role}
			if text := openAIContentText(m.Content); text != "" {
				msg.Parts = append(msg.Parts, WirePart{Type: "text", Text: text})
			}
			var images []WirePart
			var parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL *struct {
					URL string `json:"url"`
				} `json:"image_url"`
			}
			if json.Unmarshal(m.Content, &parts) == nil {
				for _, p := range parts {
					if p.Type == "image_url" && p.ImageURL != nil {
						if mime, data, ok := parseDataURL(p.ImageURL.URL); ok {
							images = append(images, WirePart{Type: "image", Mime: mime, Data: data})
						}
					}
				}
			}
			msg.Parts = append(msg.Parts, images...)
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Function.Arguments)
				if len(args) == 0 || !json.Valid(args) {
					args = json.RawMessage(`{}`)
				}
				msg.Parts = append(msg.Parts, WirePart{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Args: args})
			}
			if len(msg.Parts) > 0 {
				out = append(out, msg)
			}
		}
	}
	return system, out
}

func openAIContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// handleAuxiliaryOpenAI translates the OpenAI-shaped auxiliary endpoints
// (title generation, quick tests, the app's compaction) onto native Anthropic
// and Gemini providers. Without this, every chat using those providers failed
// at title generation even though the agent loop itself worked.
func (s *Server) handleAuxiliaryOpenAI(w http.ResponseWriter, r *http.Request, provider *Provider, modelID string, body []byte) {
	var req openAIChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	_, model, err := s.cfg.ResolveModel(modelID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	system, messages := openAIWireMessages(req.Messages)
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = req.MaxCompletion
	}
	client := s.providerFor(provider)
	if req.Stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		id := newID("chatcmpl")
		writeChunk := func(delta map[string]any, finish string) bool {
			payload, _ := json.Marshal(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": nowMillis() / 1000,
				"model":   model.ID,
				"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
			})
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}
		writeChunk(map[string]any{"role": "assistant", "content": ""}, "")
		var text strings.Builder
		_, err := client.Stream(r.Context(), ProviderRequest{
			Model: model, System: system, Messages: messages, Tools: req.Tools,
			MaxTokens: maxTokens, Thinking: "off",
		}, func(ev ProviderEvent) error {
			switch ev.Kind {
			case "text":
				text.WriteString(ev.Text)
				if !writeChunk(map[string]any{"content": ev.Text}, "") {
					return io.ErrClosedPipe
				}
			case "thinking":
				if ev.Text != "" {
					if !writeChunk(map[string]any{"reasoning_content": ev.Text}, "") {
						return io.ErrClosedPipe
					}
				}
			}
			return nil
		})
		if err != nil && r.Context().Err() == nil {
			writeChunk(map[string]any{"content": "\n\n[provider error: " + err.Error() + "]"}, "stop")
		} else {
			writeChunk(map[string]any{}, "stop")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	var text strings.Builder
	res, err := client.Stream(r.Context(), ProviderRequest{
		Model: model, System: system, Messages: messages, Tools: req.Tools,
		MaxTokens: maxTokens, Thinking: "off",
	}, func(ev ProviderEvent) error {
		if ev.Kind == "text" {
			text.WriteString(ev.Text)
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": newID("chatcmpl"), "object": "chat.completion", "created": nowMillis() / 1000,
		"model": model.ID,
		"choices": []map[string]any{{
			"index": 0, "message": map[string]any{"role": "assistant", "content": text.String()},
			"finish_reason": res.StopReason,
		}},
		"usage": map[string]any{
			"prompt_tokens": res.Usage.InputTokens, "completion_tokens": res.Usage.OutputTokens,
			"total_tokens": res.Usage.InputTokens + res.Usage.OutputTokens,
		},
	})
}
