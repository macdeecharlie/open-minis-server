package main

import (
	"bytes"
	"encoding/json"
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
	if provider.Kind == "anthropic" {
		writeError(w, http.StatusNotImplemented, "this server's Anthropic provider supports the agent endpoint only; use an OpenAI-compatible provider for auxiliary calls")
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
