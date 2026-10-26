package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type ProviderRequest struct {
	Model       *Model
	System      string
	Messages    []WireMessage
	Tools       []ToolDef
	MaxTokens   int
	Thinking    string
	IdleTimeout time.Duration
}

type ProviderClient interface {
	Stream(ctx context.Context, req ProviderRequest, emit func(ProviderEvent) error) (ProviderResult, error)
}

type providerHTTP struct {
	client *http.Client
}

func newProviderHTTP() *providerHTTP {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 150 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &providerHTTP{client: &http.Client{Transport: transport}}
}

func (p *providerHTTP) doJSON(ctx context.Context, method, url string, headers map[string]string, body any, stream bool) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	req.Header.Set("User-Agent", "open-minis-server/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return nil, &upstreamError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b)), Headers: flattenHeaders(resp.Header)}
	}
	return resp, nil
}

type upstreamError struct {
	Status  int
	Body    string
	Headers map[string]string
}

func (e *upstreamError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("provider returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("provider returned HTTP %d: %s", e.Status, e.Body)
}

// readSSE parses an SSE response and calls fn for each dispatched event.
// The returned error includes malformed data only when it matters — a
// provider that emits a non-JSON keepalive must not kill a long stream.
func readSSE(r io.Reader, fn func(event string, data []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var event string
	var data [][]byte
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 {
				payload := bytes.Join(data, []byte("\n"))
				if err := fn(event, payload); err != nil {
					return err
				}
			}
			event = ""
			data = data[:0]
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			data = append(data, []byte(d))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(data) > 0 {
		return fn(event, bytes.Join(data, []byte("\n")))
	}
	return nil
}

func openAIEndpoint(base string) string {
	b := strings.TrimSuffix(strings.TrimSpace(base), "/")
	if strings.HasSuffix(b, "/v1") {
		return b + "/chat/completions"
	}
	return b + "/v1/chat/completions"
}

func anthropicEndpoint(base string) string {
	b := strings.TrimSuffix(strings.TrimSpace(base), "/")
	if strings.HasSuffix(b, "/v1") {
		return b + "/messages"
	}
	return b + "/v1/messages"
}

func effectiveMaxTokens(m *Model, requested int) int {
	if requested > 0 {
		return requested
	}
	if m != nil && m.MaxOutputTokens > 0 {
		return m.MaxOutputTokens
	}
	return 8192
}

func wireTextAndImages(msg WireMessage) (text string, images []WirePart) {
	var b strings.Builder
	for _, p := range msg.Parts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
		case "image":
			images = append(images, p)
		}
	}
	return b.String(), images
}

func hasToolResults(msg WireMessage) bool {
	for _, p := range msg.Parts {
		if p.Type == "tool_result" {
			return true
		}
	}
	return false
}

func streamErrorRetryable(err error) bool {
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue.Status == 429 || ue.Status >= 500
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "connection reset") || strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "unexpected eof") || strings.Contains(s, "timeout") ||
		strings.Contains(s, "idle")
}

func streamErrorFallbackable(err error, strategy string) bool {
	if strings.EqualFold(strategy, "always") {
		return true
	}
	return streamErrorRetryable(err)
}

func sanitizeProviderHeaderValue(v string) string {
	// Header values are configuration, never user input; reject CR/LF just in
	// case a config file was hand-edited badly.
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

// providerIdleTimeout bounds silence on an established provider stream. A
// model that stops sending bytes without closing the connection otherwise
// hangs a run forever; the watchdog cancels the request context so the run
// fails visibly and the retry/fallback path can take over.
var providerIdleTimeout = 10 * time.Minute

type idleReader struct {
	r        io.ReadCloser
	cancel   context.CancelFunc
	first    time.Duration
	idle     time.Duration
	gotBytes bool
	timer    *time.Timer
	mu       sync.Mutex
	timedOut bool
}

func providerRequestIdle(cfg *Provider, req ProviderRequest) time.Duration {
	if req.IdleTimeout > 0 {
		return req.IdleTimeout
	}
	return cfg.idleTimeout()
}

// newIdleReader distinguishes the time-to-first-byte budget from the gap
// allowed between subsequent chunks, mirroring the app's response idle timeout.
func newIdleReader(r io.ReadCloser, cancel context.CancelFunc, first, idle time.Duration) *idleReader {
	if first <= 0 {
		first = idle
	}
	ir := &idleReader{r: r, cancel: cancel, first: first, idle: idle}
	ir.timer = time.AfterFunc(first, func() {
		ir.mu.Lock()
		ir.timedOut = true
		ir.mu.Unlock()
		cancel()
	})
	return ir
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.gotBytes = true
		r.timer.Reset(r.idle)
	}
	return n, err
}

// openAIIdle resolves the app's response idle setting: 120s to first byte by
// default, 600s between chunks, and both raised when the user's setting is
// longer. Official app semantics; Anthropic/Gemini keep their fixed 10min.
func openAIIdle(cfg *Provider, requested time.Duration) (time.Duration, time.Duration) {
	if providerIdleTimeout != 10*time.Minute {
		// Tests (and an explicit process-wide override) take precedence.
		return providerIdleTimeout, providerIdleTimeout
	}
	first := 120 * time.Second
	if requested > 0 {
		first = requested
	} else if cfg != nil && cfg.IdleTimeoutSec > 0 {
		first = cfg.idleTimeout()
	}
	if first < 30*time.Second {
		first = 30 * time.Second
	}
	if first > time.Hour {
		first = time.Hour
	}
	steady := providerIdleTimeout
	if first+30*time.Second > steady {
		steady = first + 30*time.Second
	}
	return first, steady
}

func (r *idleReader) Close() error {
	r.timer.Stop()
	return r.r.Close()
}

func (r *idleReader) TimedOut() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timedOut
}

func applyModelHeaders(headers map[string]string, model *Model) map[string]string {
	if model == nil || len(model.Headers) == 0 {
		return headers
	}
	out := make(map[string]string, len(headers)+len(model.Headers))
	for k, v := range headers {
		out[k] = v
	}
	for k, v := range model.Headers {
		out[sanitizeProviderHeaderValue(k)] = sanitizeProviderHeaderValue(v)
	}
	return out
}

// mergeExtraBody applies a model's provider-specific JSON knobs last, matching
// the app's extraBodyParams behavior (they may override generated fields).
func mergeExtraBody(body map[string]any, model *Model) {
	if model == nil || len(model.ExtraBody) == 0 {
		return
	}
	var extra map[string]any
	if json.Unmarshal(model.ExtraBody, &extra) != nil {
		return
	}
	for k, v := range extra {
		body[k] = v
	}
}

// flattenHeaders keeps response headers needed by the app's quota observers.
// Only ratelimit-style headers are forwarded: other provider headers may
// contain request ids or internal routing data the app does not need.
func flattenHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, vals := range h {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "anthropic-ratelimit-") && !strings.HasPrefix(lk, "x-codex-") &&
			!strings.HasPrefix(lk, "x-ratelimit-") {
			continue
		}
		if len(vals) > 0 {
			out[lk] = vals[0]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// azureURL mirrors the app's azureUrl(): strip /v1 or /openai from the pasted
// endpoint, keep its ?api-version query, then use the deployment path.
func azureURL(base, modelID, path string) string {
	base, query := splitQuery(base)
	p := strings.TrimSuffix(base, "/")
	if strings.HasSuffix(p, "/v1") {
		p = strings.TrimSuffix(strings.TrimSuffix(p, "/v1"), "/")
	}
	if strings.HasSuffix(p, "/openai") {
		p = strings.TrimSuffix(strings.TrimSuffix(p, "/openai"), "/")
	}
	return p + "/openai/deployments/" + url.PathEscape(modelID) + "/" + strings.TrimPrefix(path, "/") + query
}
