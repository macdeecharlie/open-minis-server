package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// toolLoopDetector is the server-side port of the official
// ToolLoopDetector. It keeps a per-run sliding window of tool calls and
// implements the same four strategies: unknown-tool streak, global
// circuit breaker, poll-no-progress and generic repeat. It also owns the
// missing tool_title reminder.
type loopConfig struct {
	historySize       int
	warningThreshold  int
	unknownThreshold  int
	criticalThreshold int
	globalThreshold   int
	titleReminderEach int
}

var defaultLoopConfig = loopConfig{
	historySize:       30,
	warningThreshold:  10,
	unknownThreshold:  10,
	criticalThreshold: 20,
	globalThreshold:   30,
	titleReminderEach: 10,
}

type loopRecord struct {
	toolName   string
	argsHash   string
	resultHash string
	unknown    string
}

type loopResult struct {
	Level   string // "" | "warning" | "critical"
	Message string
	Key     string
}

type toolLoopDetector struct {
	cfg  loopConfig
	mu   sync.Mutex
	hist []loopRecord
	// warning key -> last emitted bucket
	buckets map[string]int
	// cumulative missing-title count
	missingTitles int
	// one scheduled-fire exemption, never stacked
	exemption int
}

func newToolLoopDetector() *toolLoopDetector {
	return &toolLoopDetector{cfg: defaultLoopConfig, buckets: map[string]int{}}
}

func (d *toolLoopDetector) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hist = nil
	d.buckets = map[string]int{}
	d.missingTitles = 0
	d.exemption = 0
}

func (d *toolLoopDetector) exemptNextCall() {
	d.mu.Lock()
	d.exemption = 1
	d.mu.Unlock()
}

// hydrate reconstructs the window from stored history so a later user turn
// sees the same accumulated state as the official app. Tool results are paired
// with their tool_use by id; unmatched calls are ignored.
func (d *toolLoopDetector) hydrate(messages []WireMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	results := map[string]WirePart{}
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == "tool_result" {
				results[p.ID] = p
			}
		}
	}
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type != "tool_use" {
				continue
			}
			res, ok := results[p.ID]
			if !ok {
				continue
			}
			result, errMsg := res.Content, ""
			if res.IsError {
				errMsg, result = res.Content, ""
			}
			d.hist = append(d.hist, loopRecord{
				toolName:   p.Name,
				argsHash:   argsHash(p.Name, p.Args),
				resultHash: resultHash(result, errMsg),
				unknown:    unknownToolName(errMsg),
			})
		}
	}
	if len(d.hist) > d.cfg.historySize {
		d.hist = d.hist[len(d.hist)-d.cfg.historySize:]
	}
}

// check mirrors the official pre-execution hook. A critical result means the
// call must not run; its message becomes the model-facing tool error.
func (d *toolLoopDetector) check(name string, args json.RawMessage) loopResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	ah := argsHash(name, args)

	streak := 0
	for i := len(d.hist) - 1; i >= 0; i-- {
		rec := d.hist[i]
		if rec.unknown == "" || rec.unknown != name {
			break
		}
		streak++
	}
	if streak >= d.cfg.unknownThreshold {
		return loopResult{Level: "critical", Message: "[LOOP BLOCKED] CRITICAL: attempted unavailable tool '" + name + "' " + itoa(streak) + " times. Stop retrying that missing tool and answer without it."}
	}

	noProgress := d.noProgressStreak(name, ah)
	if noProgress >= d.cfg.globalThreshold {
		return loopResult{Level: "critical", Message: "[LOOP BLOCKED] CRITICAL: " + name + " has repeated identical no-progress outcomes " + itoa(noProgress) + " times. Session execution blocked by global circuit breaker."}
	}
	if isPollTool(name, args) {
		if noProgress >= d.cfg.criticalThreshold {
			return loopResult{Level: "critical", Message: "[LOOP BLOCKED] CRITICAL: Called " + name + " " + itoa(noProgress) + " times with identical no-progress results. Session execution blocked."}
		}
		if noProgress >= d.cfg.warningThreshold {
			return loopResult{Level: "warning", Key: "poll:" + name + ":" + ah, Message: "[LOOP WARNING] You have called " + name + " " + itoa(noProgress) + " times with no progress. Stop polling and either (1) increase wait time, or (2) report the task as failed."}
		}
	} else {
		total := 0
		for _, rec := range d.hist {
			if rec.toolName == name && rec.argsHash == ah {
				total++
			}
		}
		if total >= d.cfg.warningThreshold {
			return loopResult{Level: "warning", Key: "repeat:" + name + ":" + ah, Message: "[LOOP WARNING] You have called " + name + " " + itoa(total) + " times with identical arguments. If this is not making progress, stop retrying and report the task as failed."}
		}
	}
	return loopResult{}
}

// record appends a completed call and returns the post-record warning the
// official detector attaches to the tool result. A scheduled fire's first call
// is exempt by design.
func (d *toolLoopDetector) record(name string, args json.RawMessage, result, errMsg string) loopResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.exemption > 0 {
		d.exemption--
		return loopResult{}
	}
	ah := argsHash(name, args)
	rh := resultHash(result, errMsg)
	d.hist = append(d.hist, loopRecord{
		toolName: name, argsHash: ah, resultHash: rh, unknown: unknownToolName(errMsg),
	})
	if len(d.hist) > d.cfg.historySize {
		d.hist = d.hist[len(d.hist)-d.cfg.historySize:]
	}

	if isPollTool(name, args) {
		streak := d.noProgressStreak(name, ah)
		if streak >= d.cfg.warningThreshold && streak < d.cfg.criticalThreshold {
			key := "poll:" + name + ":" + ah
			if d.emitWarning(key, streak) {
				return loopResult{Level: "warning", Key: key, Message: "[LOOP WARNING] You have called " + name + " " + itoa(streak) + " times with no progress. Stop polling and either (1) increase wait time, or (2) report the task as failed."}
			}
		}
		return loopResult{}
	}
	total := 0
	for _, rec := range d.hist {
		if rec.toolName == name && rec.argsHash == ah {
			total++
		}
	}
	if total >= d.cfg.warningThreshold {
		key := "repeat:" + name + ":" + ah
		if d.emitWarning(key, total) {
			return loopResult{Level: "warning", Key: key, Message: "[LOOP WARNING] You have called " + name + " " + itoa(total) + " times with identical arguments. If this is not making progress, stop retrying and report the task as failed."}
		}
	}
	return loopResult{}
}

func (d *toolLoopDetector) recordToolTitle(args json.RawMessage, declared bool) string {
	if !declared || hasToolTitle(args) {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.missingTitles++
	bucket := (d.missingTitles - 1) / maxInt(d.cfg.titleReminderEach, 1)
	last, ok := d.buckets["title:missing"]
	if ok && last >= bucket {
		return ""
	}
	d.buckets["title:missing"] = bucket
	return toolTitleReminderText
}

func (d *toolLoopDetector) noProgressStreak(name, argsHash string) int {
	streak := 0
	pinned := ""
	for i := len(d.hist) - 1; i >= 0; i-- {
		rec := d.hist[i]
		if rec.toolName != name || rec.argsHash != argsHash {
			continue
		}
		if rec.resultHash == "" {
			break
		}
		if pinned == "" {
			pinned = rec.resultHash
			streak++
		} else if rec.resultHash == pinned {
			streak++
		} else {
			break
		}
	}
	return streak
}

func (d *toolLoopDetector) emitWarning(key string, count int) bool {
	bucket := count / maxInt(d.cfg.warningThreshold, 1)
	if last, ok := d.buckets[key]; ok && last == bucket {
		return false
	}
	d.buckets[key] = bucket
	return true
}

const toolTitleReminderText = "<system-reminder>[minis:tool-title-reminder] This is a soft reminder. " +
	"This tool call has no `tool_title`; please include a short one next time.</system-reminder>"

func hasToolTitle(args json.RawMessage) bool {
	var a struct {
		Title string `json:"tool_title"`
	}
	if len(args) == 0 || json.Unmarshal(args, &a) != nil {
		return false
	}
	return strings.TrimSpace(a.Title) != ""
}

func argsHash(name string, args json.RawMessage) string {
	var m map[string]any
	if len(args) == 0 || json.Unmarshal(args, &m) != nil {
		m = map[string]any{}
	}
	delete(m, "tool_title")
	b, _ := json.Marshal(m)
	sum := sha256.Sum256([]byte(name + ":" + string(b)))
	return hex.EncodeToString(sum[:])
}

func resultHash(result, errMsg string) string {
	sum := sha256.Sum256([]byte("err=" + errMsg + "out=" + result))
	return hex.EncodeToString(sum[:])
}

var (
	unknownToolRe1 = regexp.MustCompile(`(?i)unknown tool[:\s]+["']?([a-zA-Z0-9_.\-]+)["']?`)
	unknownToolRe2 = regexp.MustCompile(`(?i)tool\s+["']?([a-zA-Z0-9_.\-]+)["']?\s+(?:not found|is not available)`)
)

func unknownToolName(errMsg string) string {
	if strings.TrimSpace(errMsg) == "" {
		return ""
	}
	if m := unknownToolRe1.FindStringSubmatch(errMsg); m != nil {
		return m[1]
	}
	if m := unknownToolRe2.FindStringSubmatch(errMsg); m != nil {
		return m[1]
	}
	return ""
}

func isPollTool(name string, args json.RawMessage) bool {
	if name == "command_status" {
		return true
	}
	if name != "process" {
		return false
	}
	var a struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(args, &a) != nil {
		return false
	}
	action := strings.ToLower(a.Action)
	return action == "poll" || action == "log"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// preflightToolCall mirrors the official pre-execution validation: required
// fields must be present, non-null and non-empty (except tool_title and the
// documented file_edit.new_string deletion form). Unknown tools are left to
// the dispatcher so its "Unknown tool" message feeds the loop detector.
func preflightToolCall(name string, args json.RawMessage, tools []ToolDef) string {
	var def *ToolDef
	for i := range tools {
		if tools[i].Function.Name == name {
			def = &tools[i]
			break
		}
	}
	if def == nil {
		return ""
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if json.Unmarshal(def.Function.Parameters, &schema) != nil {
		return ""
	}
	enforced := make([]string, 0, len(schema.Required))
	for _, f := range schema.Required {
		if f != "tool_title" {
			enforced = append(enforced, f)
		}
	}
	var m map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return "Tool '" + name + "' received invalid JSON arguments."
		}
	}
	if len(m) == 0 && len(enforced) > 0 {
		return "Tool '" + name + "' was called with empty arguments {} but requires: " + strings.Join(enforced, ", ") + "."
	}
	missing := make([]string, 0, len(enforced))
	for _, f := range enforced {
		v, ok := m[f]
		if !ok || v == nil {
			missing = append(missing, f)
			continue
		}
		if s, isStr := v.(string); isStr && s == "" && !(name == "file_edit" && f == "new_string") {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return "Tool '" + name + "' is missing required parameter(s): " + strings.Join(missing, ", ") + "."
	}
	return ""
}
