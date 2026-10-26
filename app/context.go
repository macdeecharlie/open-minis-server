package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"math"
	"strings"
)

// Context estimation follows the Android client's ContextSizeMeter weight for
// weight (letters/4.5, digits/2, ASCII punctuation*0.35, one token per
// non-ASCII rune) plus wire framing. Compaction thresholds mirror
// ContextPolicy.forContextWindow: no auto-compact below 64K, then 20K of
// headroom.
const (
	perMessageOverhead     = 8
	perToolUseOverhead     = 22
	perToolResultOverhead  = 6
	reasoningFieldOverhead = 6
)

func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	letters, digits, asciiOther, nonASCII := 0, 0, 0, 0
	for _, r := range text {
		switch {
		case r >= 128:
			nonASCII++
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			letters++
		case r >= '0' && r <= '9':
			digits++
		default:
			asciiOther++
		}
	}
	return int(math.Ceil(float64(letters)/4.5 + float64(digits)/2 + float64(asciiOther)*0.35 + float64(nonASCII)))
}

func wireEscapeTokens(text string) int {
	escaped := 0
	for _, r := range text {
		if r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t' {
			escaped++
		}
	}
	return int(math.Ceil(float64(escaped) * 0.35))
}

func estimateWireTokens(text string) int {
	if text == "" {
		return 0
	}
	return estimateTextTokens(text) + wireEscapeTokens(text)
}

func estimateImageTokens(data string) int {
	if data == "" {
		return 0
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(data)
	}
	if err != nil || len(raw) == 0 {
		return 1000
	}
	w, h := imageBounds(raw)
	if w <= 0 || h <= 0 {
		return 1000
	}
	// Match BPETokenizer: scale the long edge to 2048, then a 32x32 grid,
	// with an 85-token floor.
	if w > h && w > 2048 {
		h = int(math.Ceil(float64(h) * 2048 / float64(w)))
		w = 2048
	} else if h > 2048 {
		w = int(math.Ceil(float64(w) * 2048 / float64(h)))
		h = 2048
	}
	tokens := int(math.Ceil(float64(w)/32) * math.Ceil(float64(h)/32))
	if tokens < 85 {
		tokens = 85
	}
	return tokens
}

func estimatePartTokens(p WirePart) int {
	switch p.Type {
	case "text":
		return estimateWireTokens(p.Text)
	case "tool_use":
		return estimateTextTokens(p.Name) + estimateTextTokens(p.ID) + estimateWireTokens(string(p.Args)) + perToolUseOverhead
	case "tool_result":
		return estimateWireTokens(p.Content) + estimateTextTokens(p.ID) + perToolResultOverhead + estimateImageTokens(p.Data)
	case "image", "audio":
		return estimateImageTokens(p.Data)
	default:
		return estimateWireTokens(p.Text) + estimateImageTokens(p.Data)
	}
}

func estimateMessageTokens(m WireMessage) int {
	total := perMessageOverhead
	if len(m.Parts) == 0 {
		return total
	}
	for _, p := range m.Parts {
		total += estimatePartTokens(p)
	}
	if m.ReasoningContent != "" {
		total += estimateWireTokens(m.ReasoningContent) + reasoningFieldOverhead
	}
	return total
}

func estimateMessagesTokens(messages []WireMessage) int {
	total := 0
	for _, m := range messages {
		total += estimateMessageTokens(m)
	}
	return total
}

func estimateToolsTokens(tools []ToolDef) int {
	total := 0
	for _, t := range tools {
		total += estimateTextTokens(t.Function.Name) + estimateTextTokens(t.Function.Description) + 10
		total += estimateWireTokens(string(t.Function.Parameters))
	}
	return total
}

func estimatedRequestTokens(system string, messages []WireMessage, tools []ToolDef) int {
	return estimateTextTokens(system) + estimateMessagesTokens(messages) + estimateToolsTokens(tools)
}

// compactThreshold returns 0 when the window is too small for a summary to
// help. Matches ContextPolicy.forContextWindow.
func compactThreshold(window int) int {
	switch {
	case window < 64_000:
		return 0
	case window < 128_000:
		return window - 10_000
	default:
		return window - 20_000
	}
}

const compactSystemPrompt = `You are a context compaction engine. Your summary will REPLACE the original messages in the conversation context window. The agent will read your summary as past context, then proceed based on the user's NEXT message — your summary is background, not a standing work order. Write the summary in the same language the user used in the conversation.

MUST PRESERVE (never omit or shorten):
- All file paths, directory names, URLs, UUIDs, and identifiers — copy verbatim
- Commands executed and their outcomes (success/failure/output)
- What was requested and what was done (record as past events, not as ongoing goals)
- Key decisions made and their rationale
- Errors encountered and how they were resolved
- Important constraints, rules, or user preferences mentioned
- Any tool calls and their results that affect current state

STRUCTURE:
1. Start with a one-line description of what the conversation was about (use past tense — "User asked X, agent did Y", NOT "Goal: X").
2. Then a concise narrative of what happened, preserving technical details.
3. End with a "What had been done so far" section listing completed work — NOT a "todo" or "pending" list. Do not invent ongoing objectives or carry-over tasks from old turns; if the user wants to continue, they will say so in their next message.

PRIORITIZE recent context over older history — recent decisions and recent file/path references are most useful for continuity.

Do NOT translate or alter code snippets, file paths, identifiers, or error messages. Be concise but never lose information the agent needs.`

const maxInLoopCompactions = 3

// transcriptForSummary mirrors buildConversationTextForSummary: role-prefixed
// lines with per-part caps, so a huge tool result cannot dominate the summary
// request or the summary itself.
func transcriptForSummary(messages []WireMessage) string {
	var b strings.Builder
	for _, m := range messages {
		role := strings.ToLower(m.Role)
		appendLine := func(text string) {
			text = strings.TrimSpace(text)
			if text != "" {
				b.WriteString(role)
				b.WriteString(": ")
				b.WriteString(text)
				b.WriteByte('\n')
			}
		}
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				appendLine(takeRunes(p.Text, 500))
			case "tool_use":
				b.WriteString(role + " [tool:" + p.Name + "]: " + takeRunes(string(p.Args), 200) + "\n")
			case "tool_result":
				b.WriteString(role + " [result:" + p.Name + "]: " + takeRunes(p.Content, 500) + "\n")
			case "image":
				b.WriteString(role + " [image: " + p.Mime + "]\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func takeRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// summarizeMessages performs one compaction pass. If the provider rejects the
// input as too large, the segment is split in half and summarized recursively,
// matching the Android splitter.
func (s *Server) summarizeMessages(ctx context.Context, provider ProviderClient, model *Model, messages []WireMessage, previous string, depth int, calls *int) (string, error) {
	if *calls >= 8 {
		return "", fmt.Errorf("compaction exceeded its model-call budget")
	}
	*calls++
	transcript := transcriptForSummary(messages)
	content := transcript
	if previous != "" {
		content = "Previous context summary:\n" + previous + "\n\nNew conversation to merge:\n" + transcript
	}
	// Split proactively when one summary request would consume more than half
	// the model window. The Android compaction engine does the same through
	// its segmented summarizer; waiting for a provider error wastes a round
	// trip and can fail a run that had room to compact in pieces.
	if window := model.ContextWindow; window > 0 && depth < 3 && len(messages) > 2 &&
		estimateTextTokens(content) > window/2 && *calls+2 <= 8 {
		mid := len(messages) / 2
		first, err := s.summarizeMessages(ctx, provider, model, messages[:mid], "", depth+1, calls)
		if err != nil {
			return "", err
		}
		second, err := s.summarizeMessages(ctx, provider, model, messages[mid:], "", depth+1, calls)
		if err != nil {
			return "", err
		}
		return first + "\n\n" + second, nil
	}
	compactModel := *model
	// The app's thinking_params were resolved for the user's chat level, not
	// for an OFF compaction call. Suppress them and let the provider's OFF
	// semantics apply.
	compactModel.ThinkingParams = nil
	compactModel.ThinkingParamsSet = true
	req := ProviderRequest{
		Model:  &compactModel,
		System: compactSystemPrompt,
		Messages: []WireMessage{{
			Role:  "user",
			Parts: []WirePart{{Type: "text", Text: "<conversation>\n" + content + "\n</conversation>"}},
		}},
		MaxTokens: 4096,
		Thinking:  "off",
	}
	var text strings.Builder
	_, err := provider.Stream(ctx, req, func(ev ProviderEvent) error {
		if ev.Kind == "text" {
			text.WriteString(ev.Text)
		}
		return nil
	})
	if err == nil {
		out := strings.TrimSpace(text.String())
		if out == "" {
			return "", fmt.Errorf("compaction returned an empty summary")
		}
		return out, nil
	}
	if depth >= 3 || len(messages) < 2 {
		return "", err
	}
	if *calls+2 > 8 {
		return "", err
	}
	mid := len(messages) / 2
	first, err := s.summarizeMessages(ctx, provider, model, messages[:mid], "", depth+1, calls)
	if err != nil {
		return "", err
	}
	second, err := s.summarizeMessages(ctx, provider, model, messages[mid:], "", depth+1, calls)
	if err != nil {
		return "", err
	}
	return first + "\n\n" + second, nil
}

// splitForCompaction returns the index of the first message that stays
// verbatim. It keeps the last keepTurns user turns and never splits a
// user/assistant/tool round.
func splitForCompaction(messages []WireMessage, keepTurns int) int {
	if keepTurns < 1 {
		keepTurns = 1
	}
	seen := 0
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "user" {
			continue
		}
		hasResult := false
		hasText := false
		for _, p := range m.Parts {
			if p.Type == "tool_result" {
				hasResult = true
			}
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				hasText = true
			}
		}
		if hasResult || !hasText {
			continue
		}
		seen++
		if seen >= keepTurns {
			return i
		}
	}
	return 0
}

func compactSummaryText(summary string) string {
	return "<context-summary>\n" +
		"The following is a summary of the earlier conversation that was compacted to save context space.\n" +
		"Treat it as background context only. The user's most recent message (below or in the next turn) takes precedence — if it changes the task, the goal, or any numbers/scope, follow the new instruction and do not resume the old plan from this summary. Do not re-run discovery (reading memory, scanning skills, re-reading files) unless the new instruction requires it.\n\n" +
		summary +
		"\n</context-summary>"
}

// applyCompaction replaces the older transcript with the summary inlined into
// the first retained user message, preserving strict role alternation.
func applyCompaction(messages []WireMessage, split int, summary string) []WireMessage {
	wrapped := compactSummaryText(summary)
	if split <= 0 || split >= len(messages) {
		return messages
	}
	tail := append([]WireMessage(nil), messages[split:]...)
	if len(tail) > 0 && tail[0].Role == "user" {
		parts := make([]WirePart, 0, len(tail[0].Parts)+1)
		parts = append(parts, WirePart{Type: "text", Text: wrapped})
		parts = append(parts, tail[0].Parts...)
		tail[0].Parts = parts
		return tail
	}
	return append([]WireMessage{{Role: "user", Parts: []WirePart{{Type: "text", Text: wrapped}}}}, tail...)
}

// applyStoredCompaction reuses a summary persisted by an earlier run when the
// anchor message is still present in the history. Without this every turn
// would summarize the same old span again. If the client compacted locally and
// the anchor is gone, the caller falls back to the normal threshold check.
func applyStoredCompaction(messages []WireMessage, summary, anchor string) ([]WireMessage, bool) {
	if strings.TrimSpace(summary) == "" || anchor == "" {
		return messages, false
	}
	for i, m := range messages {
		if m.ID == anchor {
			if i+1 >= len(messages) {
				return messages, false
			}
			return applyCompaction(messages, i+1, summary), true
		}
	}
	return messages, false
}

// compactSingleTurn folds the tool/assistant middle of one long user turn
// behind a summary while keeping the original user prompt. The normal split
// path needs a later user text message; the first turn, helper and scheduled
// runs often have only one, and a failing request is worse than a summary.
func compactSingleTurn(messages []WireMessage, summary string) ([]WireMessage, bool) {
	head := -1
	for i, m := range messages {
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				head = i
				break
			}
		}
		if head >= 0 {
			break
		}
	}
	if head < 0 || head+1 >= len(messages) {
		return messages, false
	}
	out := append([]WireMessage(nil), messages[:head+1]...)
	out = append(out, WireMessage{Role: "user", Parts: []WirePart{{Type: "text", Text: compactSummaryText(summary)}}})
	return out, true
}

// maybeCompact checks the outbound request and summarizes older history when
// it crosses the window's compact line. It returns the messages to send and
// whether a compaction happened.
func (s *Server) maybeCompact(run *Run, messages []WireMessage, system string, tools []ToolDef, window int, provider ProviderClient, model *Model, maxTokens int, compactions *int) ([]WireMessage, bool, error) {
	if window <= 0 || *compactions >= maxInLoopCompactions {
		return messages, false, nil
	}
	threshold := compactThreshold(window)
	if threshold <= 0 {
		return messages, false, nil
	}
	budget := window - maxTokens - 4096
	if budget > 0 && budget < threshold {
		threshold = budget
	}
	if estimatedRequestTokens(system, messages, tools) < threshold {
		return messages, false, nil
	}
	split := splitForCompaction(messages, 3)
	if split <= 0 {
		split = splitForCompaction(messages, 1)
	}
	if split <= 0 {
		// One single enormous turn: drop oversized tool-result bodies in place
		// first, then summarize the assistant/tool middle while keeping the
		// original user prompt. This is the normal first-turn/helper/scheduled
		// shape, where there is no later user turn to anchor a split.
		trimmed := trimLargeToolResults(messages, 4000)
		if estimatedRequestTokens(system, trimmed, tools) < window {
			run.emit("info", map[string]any{"text": "Context was over the model window; large tool results were shortened."})
			return trimmed, false, nil
		}
		calls := 0
		summary, err := s.summarizeMessages(run.ctx, provider, model, trimmed, "", 0, &calls)
		if err != nil {
			return messages, false, fmt.Errorf("context compaction failed: %w", err)
		}
		whole, ok := compactSingleTurn(trimmed, summary)
		if !ok {
			return messages, false, fmt.Errorf("context is full and nothing before the current turn can be compacted")
		}
		anchor := ""
		if len(trimmed) > 0 {
			anchor = trimmed[len(trimmed)-1].ID
		}
		if anchor != "" {
			_ = s.store.SetSummary(run.Session, summary, anchor)
		}
		*compactions++
		run.emit("compacted", map[string]any{
			"summary": summary, "messages": len(messages) - len(whole) + 1,
			"tokens": estimatedRequestTokens(system, whole, tools), "window": window,
			"compaction": *compactions,
		})
		return whole, true, nil
	}
	calls := 0
	summary, err := s.summarizeMessages(run.ctx, provider, model, messages[:split], "", 0, &calls)
	if err != nil {
		return messages, false, fmt.Errorf("context compaction failed: %w", err)
	}
	anchor := ""
	if split > 0 && split-1 < len(messages) {
		anchor = messages[split-1].ID
	}
	if anchor != "" {
		if err := s.store.SetSummary(run.Session, summary, anchor); err != nil {
			log.Printf("store compact summary: %v", err)
		}
	}
	compacted := applyCompaction(messages, split, summary)
	if estimatedRequestTokens(system, compacted, tools) >= window {
		compacted = trimLargeToolResults(compacted, 2000)
		run.emit("info", map[string]any{"text": "Context was still over the model window after compaction; large tool results were shortened."})
	}
	*compactions++
	run.emit("compacted", map[string]any{
		"summary":    summary,
		"anchor":     anchor,
		"messages":   len(messages) - len(compacted) + 1,
		"tokens":     estimatedRequestTokens(system, compacted, tools),
		"window":     window,
		"compaction": *compactions,
	})
	return compacted, true, nil
}

const requestImageBudget = 25 * 1024 * 1024

// applyImageBudget keeps the newest images that fit the request budget. The
// official client enforces a 25 MB per-request image budget and replaces
// older images with a text placeholder; providers reject oversized bodies, so
// this must run before every request. Data is base64, which is what travels on
// the wire.
func applyImageBudget(messages []WireMessage) ([]WireMessage, int) {
	total := 0
	dropped := 0
	out := make([]WireMessage, len(messages))
	for i := range messages {
		cp := messages[i]
		cp.Parts = append([]WirePart(nil), messages[i].Parts...)
		out[i] = cp
	}
	for i := len(out) - 1; i >= 0; i-- {
		for j := len(out[i].Parts) - 1; j >= 0; j-- {
			p := out[i].Parts[j]
			if p.Data == "" || (p.Type != "image" && p.Type != "tool_result") {
				continue
			}
			size := len(p.Data)
			if size > 10*1024*1024 || total+size > requestImageBudget {
				if p.Type == "tool_result" {
					out[i].Parts[j].Data = ""
					out[i].Parts[j].Mime = ""
					out[i].Parts[j].Content += "\n\n[image omitted: request image budget exceeded; older images are dropped first]"
				} else {
					out[i].Parts[j] = WirePart{Type: "text", Text: "[image omitted: request image budget exceeded; older images are dropped first]"}
				}
				dropped++
				continue
			}
			total += size
		}
	}
	return out, dropped
}

// trimLargeToolResults keeps tool pairing intact and only shortens bodies.
func trimLargeToolResults(messages []WireMessage, limit int) []WireMessage {
	out := make([]WireMessage, len(messages))
	for i, m := range messages {
		cp := m
		cp.Parts = append([]WirePart(nil), m.Parts...)
		for j, p := range cp.Parts {
			if p.Type == "tool_result" && len([]rune(p.Content)) > limit {
				cp.Parts[j].Content = string([]rune(p.Content)[:limit]) + "\n... (result shortened to fit the model context window)"
			}
		}
		out[i] = cp
	}
	return out
}

// repairToolPairing makes a client history safe for providers that reject
// orphaned tool calls or results. It keeps every valid pair, drops results
// whose call is absent, and fills a missing result immediately after the
// assistant turn so role alternation and the provider's "tool result follows
// tool call" requirement both hold.
func repairToolPairing(messages []WireMessage) []WireMessage {
	uses := map[string]bool{}
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == "tool_use" && p.ID != "" {
				uses[p.ID] = true
			}
		}
	}
	out := make([]WireMessage, 0, len(messages)+2)
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		if m.Role == "user" {
			parts := make([]WirePart, 0, len(m.Parts))
			for _, p := range m.Parts {
				if p.Type == "tool_result" && (p.ID == "" || !uses[p.ID]) {
					continue
				}
				parts = append(parts, p)
			}
			if len(parts) == 0 {
				continue
			}
			m.Parts = parts
			out = append(out, m)
			continue
		}
		out = append(out, m)
		if m.Role != "assistant" {
			continue
		}
		var missing []WirePart
		for _, p := range m.Parts {
			if p.Type != "tool_use" || p.ID == "" {
				continue
			}
			found := false
			windowEnd := i + 3
			if windowEnd > len(messages) {
				windowEnd = len(messages)
			}
			for j := i + 1; j < windowEnd; j++ {
				for _, q := range messages[j].Parts {
					if q.Type == "tool_result" && q.ID == p.ID {
						found = true
					}
				}
			}
			if !found {
				missing = append(missing, WirePart{
					Type: "tool_result", ID: p.ID, Name: p.Name, IsError: true,
					Content: "[tool result missing: the previous turn was interrupted before it returned]",
				})
			}
		}
		if len(missing) == 0 {
			continue
		}
		if i+1 < len(messages) && messages[i+1].Role == "user" {
			// Merge into the next user turn instead of adding a second user
			// message in a row.
			next := messages[i+1]
			next.Parts = append(append([]WirePart(nil), missing...), next.Parts...)
			out = append(out, next)
			i++
		} else {
			out = append(out, WireMessage{Role: "user", Parts: missing})
		}
	}
	return out
}
