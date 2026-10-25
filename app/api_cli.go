package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
	start := strings.TrimSpace(r.URL.Query().Get("start"))
	end := strings.TrimSpace(r.URL.Query().Get("end"))
	out := make([]map[string]any, 0, limit)
	for _, m := range s.store.ListSessions() {
		if id != "" && m.ID != id {
			continue
		}
		if start != "" {
			if t, err := time.Parse("2006-01-02", start); err == nil && m.UpdatedAt < t.UnixMilli() {
				continue
			}
		}
		if end != "" {
			if t, err := time.Parse("2006-01-02", end); err == nil && m.UpdatedAt >= t.Add(24*time.Hour).UnixMilli() {
				continue
			}
		}
		if query != "" && !s.sessionMatches(m.ID, m.Title, query) {
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

func (s *Server) sessionMatches(id, title, query string) bool {
	if strings.Contains(strings.ToLower(title), query) || strings.Contains(strings.ToLower(id), query) {
		return true
	}
	msgs, err := s.store.Messages(id)
	if err != nil {
		return false
	}
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == "text" && strings.Contains(strings.ToLower(p.Text), query) {
				return true
			}
		}
	}
	return false
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

type pendingConfigChange struct {
	Field     string
	Value     string
	CreatedAt time.Time
}

func (s *Server) handleCLIConfigSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Args []string `json:"args"`
	}
	if err := jsonDecodeLimited(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	args := req.Args
	if len(args) < 2 {
		writeError(w, http.StatusBadRequest, "usage: minis-config set default_model <model-id>")
		return
	}
	field, value := strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	if field != "default_model" && field != "defaults.default_model" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "UNSUPPORTED",
			"message": "The server currently accepts `default_model` writes through minis-config. " +
				"Other settings are edited by the user in the app.",
		})
		return
	}
	if _, _, err := s.cfg.ResolveModel(value); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	token := newID("cfg")
	s.configMu.Lock()
	// Drop expired pending changes instead of running a timer.
	for k, v := range s.pendingConfig {
		if time.Since(v.CreatedAt) > 10*time.Minute {
			delete(s.pendingConfig, k)
		}
	}
	s.pendingConfig[token] = pendingConfigChange{Field: "default_model", Value: value, CreatedAt: time.Now()}
	s.configMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "pending_confirmation",
		"token":  token,
		"change": "default_model → " + value,
		"message": "Ask the user to approve this change (user_ask), then run " +
			"`minis-config confirm --token " + token + "` in the next turn. The token expires in 10 minutes.",
	})
}

func (s *Server) handleCLIConfigConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := jsonDecodeLimited(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.configMu.Lock()
	change, ok := s.pendingConfig[req.Token]
	if ok {
		delete(s.pendingConfig, req.Token)
	}
	s.configMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "confirmation token not found or expired")
		return
	}
	if change.Field != "default_model" {
		writeError(w, http.StatusBadRequest, "unsupported pending change")
		return
	}
	s.cfg.DefaultModel = change.Value
	if err := s.persistConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, "apply config: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "default_model": change.Value})
}

// persistConfig writes the current configuration without restarting. It is
// used for settings like default_model that the running process can apply
// immediately; credential changes still go through the admin endpoint.
func (s *Server) persistConfig() error {
	if s.configPath == "" {
		return nil
	}
	body, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.configPath)
}

func parseInt(v string) (int, error) {
	var n int
	_, err := fmt.Sscanf(v, "%d", &n)
	return n, err
}
