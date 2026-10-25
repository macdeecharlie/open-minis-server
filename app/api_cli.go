package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// cliAuth protects the endpoints used by the minis-* command line tools inside
// the execution container. Those tools run as root in the sandbox and already
// hold this token, so it grants no capability the shell did not already have.
// Application clients use the separate auth token and cannot call these.
func (s *Server) cliAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := strings.TrimSpace(s.cfg.Sandbox.AgentToken)
		if want == "" {
			writeError(w, http.StatusServiceUnavailable, "sandbox CLI endpoints are disabled (no sandbox token configured)")
			return
		}
		h := r.Header.Get("Authorization")
		got := strings.TrimPrefix(h, "Bearer ")
		if got == h || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleCLISessions(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := parseInt(v); err == nil && n > 0 {
			limit = n
		}
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	out := make([]map[string]any, 0, limit)
	for _, m := range s.store.ListSessions() {
		if id != "" && m.ID != id {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(m.Title), query) && !strings.Contains(strings.ToLower(m.ID), query) {
			continue
		}
		out = append(out, map[string]any{
			"session_id":    m.ID,
			"title":         m.Title,
			"model":         m.Model,
			"message_count": m.MessageCount,
			"started_at":    time.UnixMilli(m.CreatedAt).Format(time.RFC3339),
			"last_active":   time.UnixMilli(m.UpdatedAt).Format(time.RFC3339),
		})
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) handleCLISearch(w http.ResponseWriter, r *http.Request) {
	keywords := strings.Fields(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("keywords"))))
	if len(keywords) == 0 {
		writeError(w, http.StatusBadRequest, "keywords are required")
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := parseInt(v); err == nil && n > 0 {
			limit = n
		}
	}
	results := make([]map[string]any, 0)
	for _, meta := range s.store.ListSessions() {
		msgs, err := s.store.Messages(meta.ID)
		if err != nil {
			continue
		}
		var matches []string
		for _, m := range msgs {
			for _, p := range m.Parts {
				if p.Type != "text" || p.Text == "" {
					continue
				}
				text := strings.ToLower(p.Text)
				all := true
				for _, kw := range keywords {
					if !strings.Contains(text, kw) {
						all = false
						break
					}
				}
				if all {
					matches = append(matches, truncate(p.Text, 300))
					if len(matches) >= 5 {
						break
					}
				}
			}
			if len(matches) >= 5 {
				break
			}
		}
		if len(matches) > 0 {
			results = append(results, map[string]any{
				"session_id": meta.ID, "title": meta.Title, "matches": matches,
			})
		}
		if len(results) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) handleCLISessionMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	meta, err := s.store.GetSession(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	msgs, err := s.store.Messages(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	includeTools := r.URL.Query().Get("tools") == "1"
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		item := map[string]any{"role": m.Role, "at": time.UnixMilli(m.CreatedAt).Format(time.RFC3339)}
		var text strings.Builder
		var calls []map[string]any
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				text.WriteString(p.Text)
			case "tool_use":
				call := map[string]any{"id": p.ID, "name": p.Name}
				if includeTools {
					call["input"] = json.RawMessage(p.Args)
				}
				calls = append(calls, call)
			case "tool_result":
				if includeTools {
					calls = append(calls, map[string]any{"id": p.ID, "name": p.Name, "output": p.Content, "is_error": p.IsError})
				}
			}
		}
		if text.Len() > 0 {
			item["text"] = text.String()
		}
		if len(calls) > 0 {
			item["tools"] = calls
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": meta.ID, "title": meta.Title, "model": meta.Model,
		"message_count": meta.MessageCount, "messages": out,
	})
}

func (s *Server) handleCLISessionUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Title *string `json:"title"`
	}
	if err := jsonDecodeLimited(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Title == nil || strings.TrimSpace(*req.Title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if err := s.store.UpdateSessionTitle(id, strings.TrimSpace(*req.Title)); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCLIModels(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0)
	for _, p := range s.cfg.Providers {
		if p.Kind == "mock" {
			continue
		}
		for _, m := range p.Models {
			item := map[string]any{
				"id": m.ID, "name": m.Name, "provider": p.ID,
				"context_window": m.ContextWindow, "max_output_tokens": m.MaxOutputTokens,
				"supports_reasoning": m.SupportsReasoning,
			}
			if len(m.InputModalities) > 0 {
				item["modalities"] = m.InputModalities
			}
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "default_model": s.cfg.DefaultModel})
}

type cliModelMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"`
	Parts   []WirePart      `json:"parts,omitempty"`
}

func cliMessages(raw []cliModelMessage, input json.RawMessage) ([]WireMessage, error) {
	if len(raw) == 0 && len(input) > 0 {
		var s string
		if err := json.Unmarshal(input, &s); err == nil {
			return []WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: s}}}}, nil
		}
		var list []cliModelMessage
		if err := json.Unmarshal(input, &list); err == nil && len(list) > 0 {
			raw = list
		} else {
			var one cliModelMessage
			if err := json.Unmarshal(input, &one); err == nil && one.Role != "" {
				raw = []cliModelMessage{one}
			}
		}
	}
	out := make([]WireMessage, 0, len(raw))
	for _, m := range raw {
		msg := WireMessage{Role: m.Role, Parts: append([]WirePart(nil), m.Parts...)}
		if len(msg.Parts) == 0 && len(m.Content) > 0 {
			var text string
			if err := json.Unmarshal(m.Content, &text); err == nil {
				msg.Parts = []WirePart{{Type: "text", Text: text}}
			} else {
				var parts []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL *struct {
						URL string `json:"url"`
					} `json:"image_url"`
				}
				if err := json.Unmarshal(m.Content, &parts); err == nil {
					for _, p := range parts {
						switch p.Type {
						case "text":
							msg.Parts = append(msg.Parts, WirePart{Type: "text", Text: p.Text})
						case "image_url":
							if p.ImageURL != nil {
								if mime, data, ok := parseDataURL(p.ImageURL.URL); ok {
									msg.Parts = append(msg.Parts, WirePart{Type: "image", Mime: mime, Data: data})
								}
							}
						}
					}
				}
			}
		}
		if msg.Role == "" {
			msg.Role = "user"
		}
		out = append(out, msg)
	}
	return out, nil
}

func parseDataURL(v string) (mime, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(v, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(v, prefix)
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	mime = strings.TrimSuffix(rest[:comma], ";base64")
	if mime == "" {
		mime = "image/png"
	}
	return mime, rest[comma+1:], true
}

func (s *Server) handleCLIModelUse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model     string            `json:"model"`
		Messages  []cliModelMessage `json:"messages"`
		Input     json.RawMessage   `json:"input"`
		MaxTokens int               `json:"max_tokens"`
		Thinking  string            `json:"thinking"`
	}
	if err := jsonDecodeLimited(w, r, &req, 16*1024*1024); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	provider, model, err := s.cfg.ResolveModel(req.Model)
	if err != nil {
		// Allow a unique case-insensitive name match, like minis-model-use.
		var matchedProvider *Provider
		var matchedModel *Model
		for i := range s.cfg.Providers {
			p := &s.cfg.Providers[i]
			for j := range p.Models {
				if strings.EqualFold(p.Models[j].Name, req.Model) {
					if matchedModel != nil {
						writeError(w, http.StatusBadRequest, "model name is ambiguous; pass the id")
						return
					}
					matchedProvider, matchedModel = p, &p.Models[j]
				}
			}
		}
		if matchedModel == nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		provider, model = matchedProvider, matchedModel
	}
	messages, err := cliMessages(req.Messages, req.Input)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages or input is required")
		return
	}
	client := s.providerFor(provider)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	var text strings.Builder
	res, err := client.Stream(ctx, ProviderRequest{
		Model: model, Messages: messages, MaxTokens: req.MaxTokens, Thinking: req.Thinking,
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
		"model": model.ID, "provider": provider.ID, "text": text.String(),
		"stop_reason": res.StopReason, "usage": res.Usage,
	})
}

func (s *Server) handleCLIConfigGet(w http.ResponseWriter, r *http.Request) {
	topic := strings.TrimSpace(r.URL.Query().Get("topic"))
	if topic == "" {
		topic = "defaults"
	}
	switch topic {
	case "defaults":
		writeJSON(w, http.StatusOK, map[string]any{
			"default_model": s.cfg.DefaultModel,
			"models":        s.cfg.ModelList(),
		})
	case "providers":
		out := make([]map[string]any, 0, len(s.cfg.Providers))
		for _, p := range s.cfg.Providers {
			if p.Kind == "mock" {
				continue
			}
			item := map[string]any{
				"id": p.ID, "kind": p.Kind, "base_url": p.BaseURL, "models": p.Models,
			}
			// Never echo credentials.
			out = append(out, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"providers": out})
	case "models":
		writeJSON(w, http.StatusOK, map[string]any{"models": s.cfg.ModelList(), "default_model": s.cfg.DefaultModel})
	default:
		writeError(w, http.StatusBadRequest, "unknown topic; use defaults, providers or models")
	}
}

func (s *Server) handleCLIConfigSet(w http.ResponseWriter, r *http.Request) {
	// Config writes change model/provider credentials and must be confirmed by
	// the user in the app. The app-side confirmation sheet is not exposed to
	// server-originated calls yet, so fail visibly instead of changing state
	// silently.
	writeJSON(w, http.StatusConflict, map[string]any{
		"ok":    false,
		"error": "CONFIRMATION_REQUIRED",
		"message": "Changing server configuration needs the user's approval in the OpenMinis app. " +
			"Ask the user in chat; do not retry this command.",
	})
}

func parseInt(v string) (int, error) {
	var n int
	_, err := fmt.Sscanf(v, "%d", &n)
	return n, err
}
