package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var serverToolNames = map[string]bool{
	"shell_execute": true,
	"file_read":     true,
	"file_write":    true,
	"file_edit":     true,
	"web_fetch":     true,
	"web_search":    true,
}

func toolTitle(args map[string]any) string {
	if v, ok := args["tool_title"].(string); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return ""
}

// clientOwnedCall keeps device-local files on the device. The model sees the
// normal file tools, but paths under the Android app's attachment, skill,
// offload and browser roots are executed by the paired client instead of the
// server workspace. This is what makes images, skills and browser artifacts
// remain readable in server mode without uploading every file.
// deviceCLIs are the shell commands that only exist on the Android device.
// Their implementations (native_offload handlers, Minis app services) cannot
// run in the server's Debian container, so a shell call that uses one is sent
// to the paired app instead. The official app runs these in its PRoot shell;
// routing the whole command preserves that behavior and its output format.
var deviceCLIs = map[string]bool{
	"android-a11y-cli":     true,
	"android-alarm":        true,
	"android-calendar":     true,
	"android-clipboard":    true,
	"android-contacts":     true,
	"android-device":       true,
	"android-location":     true,
	"android-notification": true,
	"android-open":         true,
	"android-photos":       true,
	"android-player":       true,
	"android-shizuku-cli":  true,
	"android-speak":        true,
	"android-speech":       true,
	"android-weather":      true,
	"minis-open":           true,
	"minis-scheduled":      true,
	// The app owns its real settings and model catalog. Routing these to the
	// paired device keeps the full official CLI surface available (appearance,
	// providers, media output) instead of the server's reduced headless copy.
	"minis-config":    true,
	"minis-model-use": true,
}

func clientOwnedCall(name string, args json.RawMessage) bool {
	if name == "shell_execute" {
		var a struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return false
		}
		return usesDeviceCLI(a.Command)
	}
	if name != "file_read" && name != "file_write" && name != "file_edit" {
		return false
	}
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return false
	}
	for _, prefix := range []string{
		"/var/minis/attachments", "/var/minis/skills",
		"/var/minis/browser",
		"/var/minis/memory", "/var/minis/shared",
		"/var/minis/mounts",
	} {
		if a.Path == prefix || strings.HasPrefix(a.Path, prefix+"/") {
			return true
		}
	}
	return false
}

func (s *Server) buildTools(client []ToolDef) []ToolDef {
	provided := map[string]ToolDef{}
	for _, t := range client {
		if t.Function.Name == "" {
			continue
		}
		provided[t.Function.Name] = t
	}
	out := make([]ToolDef, 0, len(client)+2)
	seen := map[string]bool{}
	// Keep the client's exact definitions for the four Android-compatible
	// tools, so the model sees the same contract on both platforms.
	for _, name := range []string{"shell_execute", "file_read", "file_write", "file_edit"} {
		if t, ok := provided[name]; ok {
			// The server's execution environment is a full Debian container,
			// not the phone's PRoot/Alpine sandbox. Keep the client's schema
			// and argument names, but correct the environment description so
			// the model does not avoid apt or assume PRoot limitations.
			if name == "shell_execute" {
				t.Function.Description = "Execute a command in the dedicated full Debian Linux container. The shell runs as root; apt-get/apt are available. The working directory is the session workspace. Use the client's virtual /var/minis/workspace and /var/minis/attachments paths; the sandbox maps them automatically."
			}
			out = append(out, t)
			seen[name] = true
		} else {
			out = append(out, builtinTool(name))
			seen[name] = true
		}
	}
	out = append(out, builtinTool("web_fetch"))
	seen["web_fetch"] = true
	if s.cfg.SearXNGURL != "" {
		out = append(out, builtinTool("web_search"))
		seen["web_search"] = true
	}
	// Every other tool the client offered is a client-executed tool (browser,
	// memory, user ask, sub agents, read_image, ...). It stays in the schema
	// and the server pauses for the client's result when the model calls it.
	for _, t := range client {
		name := t.Function.Name
		if name == "" || seen[name] {
			continue
		}
		// Already-added server tools keep the definition selected above.
		// Everything else is a client-executed tool and must be offered to the
		// model; dropping it here silently hid browser, memory, sub-agent and
		// device tools from every server run.
		if serverToolNames[name] {
			continue
		}
		out = append(out, t)
	}
	return out
}

func builtinTool(name string) ToolDef {
	var desc string
	var params map[string]any
	switch name {
	case "shell_execute":
		desc = "Execute a command in the remote Linux workspace. Use it for anything the file tools cannot do. The command runs as an unprivileged user in the session workspace."
		params = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_title": map[string]any{"type": "string", "description": "A concise 5-10 word summary of the call."},
				"command":    map[string]any{"type": "string", "description": "Shell command to run."},
				"timeout":    map[string]any{"type": "integer", "description": "Timeout in seconds (default 900, max 3600)."},
				"delay":      map[string]any{"type": "integer", "description": "Seconds to wait before starting (default 0)."},
			},
			"required": []string{"tool_title", "command"},
		}
	case "file_read":
		desc = "Read a text file from the session workspace. Returns a header with size/line metadata and the requested range."
		params = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_title": map[string]any{"type": "string"},
				"path":       map[string]any{"type": "string", "description": "Absolute /var/minis/workspace/... path or a path relative to the workspace."},
				"offset":     map[string]any{"type": "integer", "description": "1-based first line."},
				"lines":      map[string]any{"type": "integer", "description": "Maximum lines to return."},
				"max_length": map[string]any{"type": "integer", "description": "Maximum characters (default 15000, hard cap 80000)."},
			},
			"required": []string{"tool_title", "path"},
		}
	case "file_write":
		desc = "Write text to a file in the session workspace. Creates parent directories. Use append mode for large files."
		params = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_title":  map[string]any{"type": "string"},
				"path":        map[string]any{"type": "string"},
				"content":     map[string]any{"type": "string"},
				"append":      map[string]any{"type": "boolean"},
				"create_dirs": map[string]any{"type": "boolean"},
			},
			"required": []string{"tool_title", "path", "content"},
		}
	case "file_edit":
		desc = "Replace an exact string in a file. Read the file first. The old string must be unique unless replace_all is true."
		params = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_title":  map[string]any{"type": "string"},
				"path":        map[string]any{"type": "string"},
				"old_string":  map[string]any{"type": "string"},
				"new_string":  map[string]any{"type": "string"},
				"replace_all": map[string]any{"type": "boolean"},
			},
			"required": []string{"tool_title", "path", "old_string", "new_string"},
		}
	case "web_fetch":
		desc = "Fetch a public HTTP(S) page and return its readable text. Private/local addresses are refused."
		params = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_title": map[string]any{"type": "string"},
				"url":        map[string]any{"type": "string"},
				"max_length": map[string]any{"type": "integer", "description": "Maximum characters to return (default 30000, cap 100000)."},
			},
			"required": []string{"tool_title", "url"},
		}
	case "web_search":
		desc = "Search the web through the server's SearXNG instance and return the top results."
		params = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_title":  map[string]any{"type": "string"},
				"query":       map[string]any{"type": "string"},
				"max_results": map[string]any{"type": "integer", "description": "1-10, default 8."},
			},
			"required": []string{"tool_title", "query"},
		}
	}
	b, _ := json.Marshal(params)
	return ToolDef{Type: "function", Function: ToolFunction{Name: name, Description: desc, Parameters: b}}
}

func (s *Server) executeTool(ctx context.Context, sb ToolSandbox, name string, raw json.RawMessage, emitOutput func(string)) ToolResult {
	title := ""
	args := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return ToolResult{Name: name, Content: "Error: invalid JSON arguments: " + err.Error(), IsError: true, Title: name}
		}
	}
	title = toolTitle(args)
	if title == "" {
		title = name
	}
	res := ToolResult{Name: name, Title: title, Executor: "server"}
	start := time.Now()
	defer func() { res.DurationMs = time.Since(start).Milliseconds() }()

	str := func(k string) string { v, _ := args[k].(string); return v }
	integer := func(k string, def int) int {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return def
	}
	boolean := func(k string) bool { v, _ := args[k].(bool); return v }

	switch name {
	case "shell_execute":
		command := str("command")
		if strings.TrimSpace(command) == "" {
			res.Content, res.IsError = "Error: 'command' is required", true
			return res
		}
		timeoutSec := integer("timeout", 900)
		if timeoutSec < 1 {
			timeoutSec = 1
		}
		if timeoutSec > 3600 {
			timeoutSec = 3600
		}
		delaySec := integer("delay", 0)
		if delaySec < 0 {
			delaySec = 0
		}
		var emitted []string
		out, err := sb.RunShell(ctx, command, time.Duration(timeoutSec)*time.Second, time.Duration(delaySec)*time.Second, func(line string) {
			emitted = append(emitted, line)
			if emitOutput != nil {
				emitOutput(line)
			}
		})
		if err != nil {
			res.Content, res.IsError = "Error: "+err.Error(), true
			return res
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Exit code: %d\n", out.ExitCode)
		if out.TimedOut {
			b.WriteString("Status: timed out\n")
		}
		b.WriteString("\n")
		b.WriteString(out.Output)
		if strings.TrimSpace(out.Output) == "" {
			b.WriteString("(no output)")
		}
		res.Content = b.String()
		res.IsError = out.ExitCode != 0
	case "file_read":
		p := str("path")
		if p == "" {
			res.Content, res.IsError = "Error: 'path' is required", true
			return res
		}
		content, err := sb.ReadFile(p, integer("offset", 1), integer("lines", 0), integer("max_length", 15000))
		if err != nil {
			res.Content, res.IsError = "Error: "+err.Error(), true
			return res
		}
		res.Content = content
		if u := minisURL(p); u != "" {
			res.Content += "\n\nminis_url: " + u
		}
	case "file_write":
		p, content := str("path"), str("content")
		if p == "" {
			res.Content, res.IsError = "Error: 'path' is required", true
			return res
		}
		if len(content) > 8*1024*1024 {
			res.Content, res.IsError = "Error: content exceeds the 8 MB server limit", true
			return res
		}
		if err := sb.WriteFile(p, content, boolean("append"), boolean("create_dirs")); err != nil {
			res.Content, res.IsError = "Error: "+err.Error(), true
			return res
		}
		res.Content = fmt.Sprintf("Wrote %d bytes to %s", len(content), p)
		if u := minisURL(p); u != "" {
			res.Content += "\nminis_url: " + u
		}
	case "file_edit":
		p, oldS, newS := str("path"), str("old_string"), str("new_string")
		if p == "" {
			res.Content, res.IsError = "Error: 'path' is required", true
			return res
		}
		if oldS == "" {
			res.Content, res.IsError = "Error: 'old_string' is required", true
			return res
		}
		n, err := sb.EditFile(p, oldS, newS, boolean("replace_all"))
		if err != nil {
			res.Content, res.IsError = "Error: "+err.Error(), true
			return res
		}
		res.Content = fmt.Sprintf("Replaced %d occurrence(s) in %s", n, p)
		if u := minisURL(p); u != "" {
			res.Content += "\nminis_url: " + u
		}
	case "web_fetch":
		u := str("url")
		if u == "" {
			res.Content, res.IsError = "Error: 'url' is required", true
			return res
		}
		text, err := safeFetch(ctx, u, integer("max_length", 30000))
		if err != nil {
			res.Content, res.IsError = "Error: "+err.Error(), true
			return res
		}
		res.Content = text
	case "web_search":
		q := str("query")
		if q == "" {
			res.Content, res.IsError = "Error: 'query' is required", true
			return res
		}
		text, err := s.search(ctx, q, integer("max_results", 8))
		if err != nil {
			res.Content, res.IsError = "Error: "+err.Error(), true
			return res
		}
		res.Content = text
	default:
		res.Content, res.IsError = "Unknown server tool: "+name, true
	}
	return res
}

var tagRE = regexp.MustCompile(`(?s)<[^>]*>`)
var scriptRE = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
var styleRE = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
var spaceRE = regexp.MustCompile(`[ \t]+`)
var blankRE = regexp.MustCompile(`\n{3,}`)
var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func htmlToText(body string) (string, string) {
	title := ""
	if m := titleRE.FindStringSubmatch(body); len(m) > 1 {
		title = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	body = scriptRE.ReplaceAllString(body, " ")
	body = styleRE.ReplaceAllString(body, " ")
	body = strings.ReplaceAll(body, "<br", "\n<br")
	body = strings.ReplaceAll(body, "</p>", "\n")
	body = strings.ReplaceAll(body, "</div>", "\n")
	body = strings.ReplaceAll(body, "</li>", "\n")
	body = tagRE.ReplaceAllString(body, " ")
	body = html.UnescapeString(body)
	body = strings.ReplaceAll(body, "\r", "")
	body = spaceRE.ReplaceAllString(body, " ")
	body = blankRE.ReplaceAllString(body, "\n\n")
	return title, strings.TrimSpace(body)
}

func safeFetch(ctx context.Context, rawURL string, maxLen int) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("only http and https URLs are allowed")
	}
	if maxLen <= 0 || maxLen > 100_000 {
		maxLen = 30_000
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         safeDialContext,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return checkPublicHost(req.Context(), req.URL.Hostname())
		},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "open-minis-server/1.0 (+read-only fetch)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return "", err
	}
	var text string
	if strings.Contains(ct, "html") {
		title, t := htmlToText(string(body))
		text = t
		if title != "" {
			text = "# " + title + "\n\n" + text
		}
	} else {
		text = string(body)
	}
	if len(text) > maxLen {
		text = text[:maxLen] + "\n… (truncated by server)"
	}
	return fmt.Sprintf("URL: %s\nStatus: %d\n\n%s", rawURL, resp.StatusCode, text), nil
}

func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if err := checkPublicHost(ctx, host); err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(host, port))
}

func checkPublicHost(ctx context.Context, host string) error {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return errors.New("host did not resolve")
	}
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() || ip.IP.IsUnspecified() {
			return fmt.Errorf("refusing private or local address %s", ip.IP)
		}
	}
	return nil
}

func (s *Server) search(ctx context.Context, query string, limit int) (string, error) {
	if s.cfg.SearXNGURL == "" {
		return "", errors.New("web_search is not configured")
	}
	if limit < 1 {
		limit = 8
	}
	if limit > 10 {
		limit = 10
	}
	endpoint := strings.TrimSuffix(s.cfg.SearXNGURL, "/") + "/search"
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("language", "en")
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("SearXNG HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&payload); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Search: " + query + "\n\n")
	for i, r := range payload.Results {
		if i >= limit {
			break
		}
		fmt.Fprintf(&b, "%d. %s\n%s\n%s\n\n", i+1, r.Title, r.URL, strings.TrimSpace(r.Content))
	}
	if b.Len() == len("Search: "+query+"\n\n") {
		return "No results.", nil
	}
	return b.String(), nil
}

// usesDeviceCLI reports whether any command segment invokes a CLI that only
// exists on the Android device. It is intentionally lexical: quote-aware
// splitting plus wrapper stripping, no shell expansion.
func usesDeviceCLI(command string) bool {
	if usesDevicePath(command) {
		return true
	}
	for _, segment := range splitShellSegments(command) {
		words := shellWords(segment)
		words = stripShellWrappers(words)
		if len(words) == 0 {
			continue
		}
		base := words[0]
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		if deviceCLIs[base] {
			return true
		}
	}
	return false
}

// devicePaths back directories that live on the phone (memory, skills,
// mounted SAF folders, browser artifacts, attachments). Shell commands that
// touch them must run on the device, otherwise the model sees a different
// filesystem than the file tools and the app UI.
func usesDevicePath(command string) bool {
	for _, prefix := range []string{
		"/var/minis/attachments", "/var/minis/skills", "/var/minis/browser",
		"/var/minis/memory", "/var/minis/shared",
		"/var/minis/mounts",
	} {
		if strings.Contains(command, prefix) {
			return true
		}
	}
	return false
}

func splitShellSegments(command string) []string {
	var out []string
	var b strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}
	rs := []rune(command)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if quote != 0 {
			b.WriteRune(r)
			if r == '\\' && quote == '"' {
				escaped = true
			} else if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			b.WriteRune(r)
		case '\\':
			escaped = true
			b.WriteRune(r)
		case ';', '|', '&', '\n', '(', ')':
			flush()
		case '#':
			// A comment runs to the end of the line.
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

func shellWords(segment string) []string {
	var out []string
	var b strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range segment {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if quote != 0 {
			if r == '\\' && quote == '"' {
				escaped = true
				continue
			}
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case '\\':
			escaped = true
		case ' ', '\t':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

func stripShellWrappers(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		if isEnvAssignment(w) {
			words = words[1:]
			continue
		}
		switch w {
		case "sudo", "env", "command", "exec", "nohup", "nice", "stdbuf":
			words = words[1:]
			continue
		case "timeout":
			words = words[1:]
			if len(words) > 0 && !strings.HasPrefix(words[0], "-") {
				words = words[1:]
			}
			continue
		}
		return words
	}
	return words
}

func isEnvAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	return i > 0 && !strings.ContainsAny(w[:i], "/$`()[]{}")
}

// minisURL maps a virtual workspace path to the app's minis:// scheme,
// percent-encoding each segment. Tool results include it so the model can
// embed a tappable link without hand-encoding non-ASCII names.
func minisURL(p string) string {
	prefixes := []struct{ from, to string }{
		{"/var/minis/workspace/", "workspace/"},
		{"/var/minis/attachments/", "attachments/"},
		{"/var/minis/offloads/", "offloads/"},
		{"/var/minis/browser/", "browser/"},
		{"/var/minis/shared/", "shared/"},
		{"/var/minis/skills/", "skills/"},
		{"/var/minis/memory/", "memory/"},
	}
	for _, pfx := range prefixes {
		if !strings.HasPrefix(p, pfx.from) {
			continue
		}
		rest := strings.TrimPrefix(p, pfx.from)
		segs := strings.Split(rest, "/")
		for i, seg := range segs {
			segs[i] = url.PathEscape(seg)
		}
		return "minis://" + pfx.to + strings.Join(segs, "/")
	}
	return ""
}
