package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// The minis-* tools are the agent-facing CLI surface that used to live inside
// the phone's PRoot rootfs. In the client-server architecture their data lives
// on the server, so the same names are provided inside the Debian execution
// container and call the server's /v1/cli API with the sandbox token.

type cliClient struct {
	base  string
	token string
	http  *http.Client
}

func newCLIClient() *cliClient {
	base := strings.TrimSpace(os.Getenv("MINIS_CLI_URL"))
	if base == "" {
		base = "http://open-minis-server:8080"
	}
	token := strings.TrimSpace(os.Getenv("MINIS_CLI_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("MINIS_SANDBOX_TOKEN"))
	}
	return &cliClient{
		base:  strings.TrimSuffix(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 15 * time.Minute},
	}
}

func (c *cliClient) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			msg = strings.TrimSpace(e.Error)
		}
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("server HTTP %d: %s", resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

type cliOutput struct {
	quiet bool
}

func (o cliOutput) print(v any) {
	var b []byte
	if o.quiet {
		b, _ = json.Marshal(v)
	} else {
		b, _ = json.MarshalIndent(v, "", "  ")
	}
	fmt.Println(string(b))
}

func (o cliOutput) fail(code, msg string) int {
	o.print(map[string]any{"ok": false, "error": map[string]any{"code": code, "message": msg}})
	return 1
}

func runCLI(program string, argv []string) int {
	client := newCLIClient()
	switch program {
	case "minis-sessions-cli":
		return runSessionsCLI(client, argv)
	case "minis-config":
		return runConfigCLI(client, argv)
	case "minis-model-use":
		return runModelUseCLI(client, argv)
	case "minis-scheduled":
		return runScheduledCLI(client, argv)
	default:
		fmt.Fprintf(os.Stderr, "unknown minis CLI %q\n", program)
		return 2
	}
}

func newFlagSet(name string) (*flag.FlagSet, *cliOutput) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	out := &cliOutput{}
	fs.BoolVar(&out.quiet, "compact", false, "compact JSON output")
	fs.BoolVar(&out.quiet, "q", false, "compact JSON output")
	fs.BoolVar(&out.quiet, "quiet", false, "compact JSON output")
	return fs, out
}

func helpOrError(fs *flag.FlagSet, argv []string, help string) (bool, int) {
	for _, a := range argv {
		if a == "--help" || a == "-h" || a == "help" {
			fmt.Print(help)
			return true, 0
		}
	}
	if err := fs.Parse(argv); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return true, 2
	}
	return false, 0
}

const sessionsHelp = `Usage: minis-sessions-cli <command> [options]

Commands:
  list [--limit N] [--id ID] [--keywords WORDS]  List sessions
  search --keywords WORDS                         Search message text
  messages --id ID [--tools]                      Read one transcript
  status --id ID                                  Session state
  update --id ID --title TITLE                    Change a session title
  groups                                          List groups
`

func runSessionsCLI(c *cliClient, argv []string) int {
	sub := "list"
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		sub = argv[0]
		argv = argv[1:]
	}
	fs, out := newFlagSet("minis-sessions-cli")
	limit := fs.Int("limit", 20, "maximum sessions")
	id := fs.String("id", "", "session id")
	keywords := fs.String("keywords", "", "keywords")
	start := fs.String("start", "", "start date YYYY-MM-DD")
	end := fs.String("end", "", "end date YYYY-MM-DD")
	group := fs.String("group", "", "group name")
	tools := fs.Bool("tools", false, "include tool calls and outputs")
	title := fs.String("title", "", "new title")
	maxItems := fs.Int("max", 0, "maximum sessions")
	if done, code := helpOrError(fs, argv, sessionsHelp); done {
		return code
	}
	switch sub {
	case "list":
		q := url.Values{}
		if *limit > 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		if *id != "" {
			q.Set("id", *id)
		}
		if *keywords != "" {
			q.Set("q", *keywords)
		}
		if *start != "" {
			q.Set("start", *start)
		}
		if *end != "" {
			q.Set("end", *end)
		}
		if *group != "" {
			q.Set("group", *group)
		}
		if *maxItems > 0 {
			q.Set("limit", strconv.Itoa(*maxItems))
		}
		var res map[string]any
		if err := c.do("GET", "/v1/cli/sessions?"+q.Encode(), nil, &res); err != nil {
			return out.fail("INTERNAL", err.Error())
		}
		out.print(res)
		return 0
	case "search":
		if strings.TrimSpace(*keywords) == "" {
			return out.fail("INVALID_ARGS", "--keywords is required")
		}
		q := url.Values{}
		q.Set("keywords", *keywords)
		if *limit > 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		var res map[string]any
		if err := c.do("GET", "/v1/cli/search?"+q.Encode(), nil, &res); err != nil {
			return out.fail("INTERNAL", err.Error())
		}
		out.print(res)
		return 0
	case "messages":
		if *id == "" {
			return out.fail("INVALID_ARGS", "--id is required")
		}
		q := url.Values{}
		if *tools {
			q.Set("tools", "1")
		}
		var res map[string]any
		if err := c.do("GET", "/v1/cli/sessions/"+url.PathEscape(*id)+"/messages?"+q.Encode(), nil, &res); err != nil {
			return out.fail("NOT_FOUND", err.Error())
		}
		out.print(res)
		return 0
	case "status":
		if *id == "" {
			return out.fail("INVALID_ARGS", "--id is required")
		}
		q := url.Values{}
		q.Set("id", *id)
		var res map[string]any
		if err := c.do("GET", "/v1/cli/sessions?"+q.Encode(), nil, &res); err != nil {
			return out.fail("INTERNAL", err.Error())
		}
		sessions, _ := res["sessions"].([]any)
		if len(sessions) == 0 {
			return out.fail("NOT_FOUND", "session not found")
		}
		out.print(map[string]any{"ok": true, "session": sessions[0], "status": "idle"})
		return 0
	case "update":
		if *id == "" || strings.TrimSpace(*title) == "" {
			return out.fail("INVALID_ARGS", "--id and --title are required")
		}
		var res map[string]any
		if err := c.do("POST", "/v1/cli/sessions/"+url.PathEscape(*id)+"/update", map[string]any{"title": *title}, &res); err != nil {
			return out.fail("NOT_FOUND", err.Error())
		}
		out.print(map[string]any{"ok": true, "session_id": *id, "title": *title})
		return 0
	case "groups":
		out.print(map[string]any{"ok": true, "groups": []any{}})
		return 0
	default:
		return out.fail("INVALID_ARGS", "Unknown command '"+sub+"'. Valid: list, search, messages, status, update, groups.")
	}
}

const configHelp = `Usage: minis-config <command> [options]

Commands:
  get [defaults|providers|models]   Read server configuration (secrets omitted)
  set default_model <id>            Propose a change; returns a confirm token
  confirm --token <token>           Apply a change the user approved
`

func runConfigCLI(c *cliClient, argv []string) int {
	sub := "get"
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		sub = argv[0]
		argv = argv[1:]
	}
	fs, out := newFlagSet("minis-config")
	filter := fs.String("filter", "", "filter array fields")
	page := fs.Int("page", 0, "page number")
	pageSize := fs.Int("page-size", 0, "page size")
	confirmToken := fs.String("token", "", "confirmation token")
	if sub == "confirm" {
		if done, code := helpOrError(fs, argv, configHelp); done {
			return code
		}
		if *confirmToken == "" {
			return out.fail("INVALID_ARGS", "--token is required")
		}
		var res map[string]any
		if err := c.do("POST", "/v1/cli/config/confirm", map[string]any{"token": *confirmToken}, &res); err != nil {
			return out.fail("NOT_FOUND", err.Error())
		}
		out.print(res)
		return 0
	}
	if sub == "set" {
		if len(argv) == 0 || argv[0] == "--help" || argv[0] == "-h" {
			fmt.Print(configHelp)
			return 0
		}
		var res map[string]any
		err := c.do("POST", "/v1/cli/config", map[string]any{"args": argv}, &res)
		if err != nil {
			return out.fail("CONFIG_ERROR", err.Error())
		}
		out.print(res)
		return 0
	}
	if done, code := helpOrError(fs, argv, configHelp); done {
		return code
	}
	switch sub {
	case "topic-help":
		topic := "defaults"
		if fs.NArg() > 0 {
			topic = fs.Arg(0)
		}
		fmt.Printf("Topics: defaults, providers, models. Secrets are never returned.\nRequested: %s\n", topic)
		return 0
	case "get":
		_, _, _ = *filter, *page, *pageSize
		topic := "defaults"
		if fs.NArg() > 0 {
			topic = fs.Arg(0)
		} else if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
			topic = argv[0]
		}
		q := url.Values{}
		q.Set("topic", topic)
		var res map[string]any
		if err := c.do("GET", "/v1/cli/config?"+q.Encode(), nil, &res); err != nil {
			return out.fail("INVALID_ARGS", err.Error())
		}
		out.print(res)
		return 0
	case "set":
		var res map[string]any
		body := map[string]any{"args": fs.Args()}
		err := c.do("POST", "/v1/cli/config", body, &res)
		if err != nil {
			return out.fail("CONFIRMATION_REQUIRED", err.Error())
		}
		out.print(res)
		return 0
	default:
		return out.fail("INVALID_ARGS", "Unknown command '"+sub+"'. Valid: get, set.")
	}
}

const modelUseHelp = `Usage: minis-model-use <command> [options]

Commands:
  list                                   Models available to the agent
  search QUERY                           Filter by id, name or provider
  run --model ID_OR_NAME [--input FILE]  Send an OpenAI-shaped messages request
      [--output FILE] [--max-tokens N] [--thinking off|low|medium|high]
`

func runModelUseCLI(c *cliClient, argv []string) int {
	sub := "list"
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		sub = argv[0]
		argv = argv[1:]
	}
	fs, out := newFlagSet("minis-model-use")
	model := fs.String("model", "", "model id or name")
	input := fs.String("input", "", "JSON input file (default stdin when piped)")
	output := fs.String("output", "", "write result text to a file")
	maxTokens := fs.Int("max-tokens", 0, "max output tokens")
	thinking := fs.String("thinking", "", "thinking level")
	if done, code := helpOrError(fs, argv, modelUseHelp); done {
		return code
	}
	switch sub {
	case "list", "search":
		var res struct {
			Models       []map[string]any `json:"models"`
			DefaultModel string           `json:"default_model"`
		}
		if err := c.do("GET", "/v1/cli/models", nil, &res); err != nil {
			return out.fail("INTERNAL", err.Error())
		}
		models := res.Models
		if sub == "search" {
			q := strings.ToLower(strings.Join(fs.Args(), " "))
			filtered := make([]map[string]any, 0)
			for _, m := range models {
				blob := strings.ToLower(fmt.Sprint(m))
				if q == "" || strings.Contains(blob, q) {
					filtered = append(filtered, m)
				}
			}
			models = filtered
		}
		out.print(map[string]any{"ok": true, "models": models, "default_model": res.DefaultModel})
		return 0
	case "run":
		if *model == "" {
			return out.fail("INVALID_ARGS", "--model is required")
		}
		var raw []byte
		var err error
		if *input != "" {
			raw, err = os.ReadFile(*input)
		} else {
			st, _ := os.Stdin.Stat()
			if st != nil && st.Mode()&os.ModeCharDevice == 0 {
				raw, err = io.ReadAll(io.LimitReader(os.Stdin, 16*1024*1024))
			}
		}
		if err != nil {
			return out.fail("INVALID_ARGS", "read input: "+err.Error())
		}
		req := map[string]any{"model": *model}
		if len(bytes.TrimSpace(raw)) > 0 {
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				return out.fail("INVALID_ARGS", "input is not a JSON object: "+err.Error())
			}
			for k, v := range obj {
				req[k] = v
			}
			req["model"] = *model
		}
		if *maxTokens > 0 {
			req["max_tokens"] = *maxTokens
		}
		if *thinking != "" {
			req["thinking"] = *thinking
		}
		var res struct {
			Text       string `json:"text"`
			Model      string `json:"model"`
			Provider   string `json:"provider"`
			StopReason string `json:"stop_reason"`
			Usage      any    `json:"usage"`
		}
		if err := c.do("POST", "/v1/cli/model-use", req, &res); err != nil {
			return out.fail("INTERNAL", err.Error())
		}
		if *output != "" {
			if err := os.WriteFile(*output, []byte(res.Text), 0o644); err != nil {
				return out.fail("INTERNAL", "write output: "+err.Error())
			}
			out.print(map[string]any{"ok": true, "model": res.Model, "provider": res.Provider, "output": *output, "bytes": len(res.Text), "usage": res.Usage})
			return 0
		}
		fmt.Println(res.Text)
		return 0
	default:
		return out.fail("INVALID_ARGS", "Unknown command '"+sub+"'. Valid: list, search, run.")
	}
}

const scheduledHelp = `Usage: minis-scheduled <command> [options]

Commands:
  create --prompt TEXT [--label L] [--after 30m | --interval 10m [--count N] |
         --time HH:MM [--repeat once|daily|weekdays|custom] [--days mon,tue]]
         [--target new|append --session ID] [--model ID] [--thinking LEVEL]
  list
  delete --id ID
  enable --id ID
  disable --id ID
  run --id ID
`

func runScheduledCLI(c *cliClient, argv []string) int {
	sub := "list"
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		sub = argv[0]
		argv = argv[1:]
	}
	fs, out := newFlagSet("minis-scheduled")
	prompt := fs.String("prompt", "", "prompt to run")
	label := fs.String("label", "", "task label")
	after := fs.String("after", "", "run once after a delay (e.g. 30m)")
	interval := fs.String("interval", "", "repeat interval (e.g. 10m)")
	timeOfDay := fs.String("time", "", "time of day HH:MM")
	repeat := fs.String("repeat", "once", "once|daily|weekdays|custom")
	days := fs.String("days", "", "comma-separated weekdays")
	count := fs.Int("count", 0, "maximum number of fires")
	target := fs.String("target", "new", "new|append")
	session := fs.String("session", "", "target session id for append")
	model := fs.String("model", "", "model id")
	thinking := fs.String("thinking", "", "thinking level")
	id := fs.String("id", "", "task id")
	if done, code := helpOrError(fs, argv, scheduledHelp); done {
		return code
	}
	switch sub {
	case "create":
		if strings.TrimSpace(*prompt) == "" {
			return out.fail("INVALID_ARGS", "--prompt is required")
		}
		task := map[string]any{
			"label": *label, "prompt": *prompt, "target": *target,
			"enabled": true, "max_fires": *count,
			"model": *model, "thinking": *thinking,
		}
		if task["label"] == "" {
			task["label"] = truncate(*prompt, 40)
		}
		switch {
		case *after != "":
			d, err := time.ParseDuration(*after)
			if err != nil {
				return out.fail("INVALID_ARGS", "invalid --after: "+err.Error())
			}
			task["kind"] = "once"
			task["at"] = time.Now().Add(d).UnixMilli()
		case *interval != "":
			d, err := time.ParseDuration(*interval)
			if err != nil {
				return out.fail("INVALID_ARGS", "invalid --interval: "+err.Error())
			}
			task["kind"] = "interval"
			task["interval_sec"] = int64(d / time.Second)
		case *timeOfDay != "":
			task["time"] = *timeOfDay
			switch *repeat {
			case "once":
				task["kind"] = "daily"
				task["max_fires"] = 1
			case "daily":
				task["kind"] = "daily"
			case "weekdays":
				task["kind"] = "weekdays"
			case "custom":
				task["kind"] = "custom"
				if *days == "" {
					return out.fail("INVALID_ARGS", "--days is required for --repeat custom")
				}
				parsed := []int{}
				for _, d := range strings.Split(*days, ",") {
					if n, ok := weekdayNumber(strings.TrimSpace(d)); ok {
						parsed = append(parsed, n)
					} else {
						return out.fail("INVALID_ARGS", "invalid weekday "+d)
					}
				}
				task["days"] = parsed
			default:
				return out.fail("INVALID_ARGS", "--repeat must be once, daily, weekdays or custom")
			}
		default:
			return out.fail("INVALID_ARGS", "one of --after, --interval or --time is required")
		}
		if *target == "append" {
			if *session == "" {
				return out.fail("INVALID_ARGS", "--session is required for --target append")
			}
			task["session_id"] = *session
		}
		var res map[string]any
		if err := c.do("POST", "/v1/cli/scheduled", task, &res); err != nil {
			return out.fail("INVALID_ARGS", err.Error())
		}
		out.print(res)
		return 0
	case "list":
		var res map[string]any
		if err := c.do("GET", "/v1/cli/scheduled", nil, &res); err != nil {
			return out.fail("INTERNAL", err.Error())
		}
		out.print(res)
		return 0
	case "delete":
		if *id == "" {
			return out.fail("INVALID_ARGS", "--id is required")
		}
		if err := c.do("DELETE", "/v1/cli/scheduled/"+url.PathEscape(*id), nil, nil); err != nil {
			return out.fail("NOT_FOUND", err.Error())
		}
		out.print(map[string]any{"ok": true, "deleted": *id})
		return 0
	case "enable", "disable":
		if *id == "" {
			return out.fail("INVALID_ARGS", "--id is required")
		}
		var list struct {
			Tasks []ScheduledTask `json:"tasks"`
		}
		if err := c.do("GET", "/v1/cli/scheduled", nil, &list); err != nil {
			return out.fail("INTERNAL", err.Error())
		}
		for _, t := range list.Tasks {
			if t.ID == *id {
				t.Enabled = sub == "enable"
				var res map[string]any
				if err := c.do("POST", "/v1/cli/scheduled", t, &res); err != nil {
					return out.fail("INTERNAL", err.Error())
				}
				out.print(res)
				return 0
			}
		}
		return out.fail("NOT_FOUND", "task not found")
	case "run":
		if *id == "" {
			return out.fail("INVALID_ARGS", "--id is required")
		}
		var res map[string]any
		if err := c.do("POST", "/v1/cli/scheduled/"+url.PathEscape(*id)+"/run", map[string]any{}, &res); err != nil {
			return out.fail("NOT_FOUND", err.Error())
		}
		out.print(res)
		return 0
	default:
		return out.fail("INVALID_ARGS", "Unknown command '"+sub+"'. Valid: create, list, delete, enable, disable, run.")
	}
}

func weekdayNumber(s string) (int, bool) {
	switch strings.ToLower(s) {
	case "sun", "sunday":
		return 0, true
	case "mon", "monday":
		return 1, true
	case "tue", "tuesday":
		return 2, true
	case "wed", "wednesday":
		return 3, true
	case "thu", "thursday":
		return 4, true
	case "fri", "friday":
		return 5, true
	case "sat", "saturday":
		return 6, true
	}
	return 0, false
}
