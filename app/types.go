package main

import (
	"encoding/json"
	"time"
)

// WireMessage is the client-facing conversation shape. It mirrors the
// AgentMessage type in the Android client so the client can send its existing
// history without translating it into an OpenAI/Anthropic specific form.
type WireMessage struct {
	ID               string     `json:"id,omitempty"`
	Role             string     `json:"role"`
	Parts            []WirePart `json:"parts"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
}

// WirePart is one ordered part of a message.
//
//	text        Text
//	image       Mime + Data (base64)
//	tool_use    ID + Name + Args
//	tool_result ID + Name + Content + IsError
type WirePart struct {
	Type             string          `json:"type"`
	Text             string          `json:"text,omitempty"`
	Mime             string          `json:"mime,omitempty"`
	Data             string          `json:"data,omitempty"`
	Format           string          `json:"format,omitempty"`
	ID               string          `json:"id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Args             json.RawMessage `json:"args,omitempty"`
	Content          string          `json:"content,omitempty"`
	IsError          bool            `json:"is_error,omitempty"`
	ThoughtSignature string          `json:"thought_signature,omitempty"`
}

// ToolDef is the OpenAI function-tool shape. The Android client already
// produces this shape from its AgentToolDefinition model.
type ToolDef struct {
	Type     string       `json:"type,omitempty"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// RunRequest starts one client-visible agent run. History is sent in full on
// every request so an edit, truncation or compaction on the client can never
// leave the server with a divergent transcript.
type RunRequest struct {
	RunID            string         `json:"run_id"`
	SessionID        string         `json:"session_id"`
	Model            string         `json:"model"`
	System           string         `json:"system"`
	Messages         []WireMessage  `json:"messages"`
	Tools            []ToolDef      `json:"tools"`
	MaxTurns         int            `json:"max_turns"`
	MaxTokens        int            `json:"max_tokens"`
	Thinking         string         `json:"thinking"`
	Prompt           *PromptContext `json:"prompt_context,omitempty"`
	Fallbacks        []string       `json:"fallbacks,omitempty"`
	FallbackStrategy string         `json:"fallback_strategy,omitempty"`
	UserMessageID    string         `json:"user_message_id"`
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	ReasoningTokens          int `json:"reasoning_tokens,omitempty"`
	LatestContextTokens      int `json:"latest_context_tokens,omitempty"`
}

type ProviderEvent struct {
	Kind      string
	Text      string
	ID        string
	Name      string
	Args      string
	Signature string
	Usage     *Usage
}

type ProviderResult struct {
	StopReason string
	Usage      Usage
}

type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

type ToolResult struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
	Title      string `json:"title,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Executor   string `json:"executor,omitempty"`
	// Image-bearing tool results (browser screenshots, read_image). The bytes
	// travel as base64 so the same result can be replayed to a vision model on
	// the next turn.
	Image     string `json:"image,omitempty"`
	ImageMime string `json:"image_mime,omitempty"`
	ImagePath string `json:"image_path,omitempty"`
}

type StoredMessage struct {
	ID               string           `json:"id"`
	SessionID        string           `json:"session_id"`
	Role             string           `json:"role"`
	Parts            []WirePart       `json:"parts"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	Usage            *Usage           `json:"usage,omitempty"`
	DurationsMs      map[string]int64 `json:"durations_ms,omitempty"`
	CreatedAt        int64            `json:"created_at"`
}

type SessionMeta struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Model        string `json:"model,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
	MessageCount int    `json:"message_count"`
	// Compacted history is stored as a summary anchored to the last message
	// it covers, so the next run reuses it instead of summarizing again.
	Summary         string `json:"summary,omitempty"`
	SummaryAnchorID string `json:"summary_anchor_id,omitempty"`
	SummaryAt       int64  `json:"summary_at,omitempty"`
}

type Event struct {
	Name string
	Data any
}

func nowMillis() int64 { return time.Now().UnixMilli() }
