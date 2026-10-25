package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// mockProvider is an explicitly configured, offline provider. It exists to
// verify the whole client/server path (streaming, persistence, tools) without
// an upstream API key. It never silently replaces a real provider.
type mockProvider struct{}

func (m *mockProvider) Stream(ctx context.Context, req ProviderRequest, emit func(ProviderEvent) error) (ProviderResult, error) {
	last := ""
	var lastParts []WirePart
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastParts = req.Messages[i].Parts
			for _, p := range lastParts {
				if p.Type == "text" {
					last += p.Text
				}
			}
			break
		}
	}
	for _, p := range lastParts {
		if p.Type == "tool_result" {
			reply := fmt.Sprintf("Tool `%s` finished.\n\n```\n%s\n```\n\n_This is the offline mock provider; configure a real provider in /data/config.json._", p.Name, truncate(p.Content, 4000))
			if err := streamText(ctx, emit, reply); err != nil {
				return ProviderResult{}, err
			}
			return ProviderResult{StopReason: "end_turn", Usage: Usage{InputTokens: 20, OutputTokens: len(reply) / 4, LatestContextTokens: 20}}, nil
		}
	}
	if cmd, ok := mockShellCommand(last); ok {
		id := "call_mock_shell"
		if err := emit(ProviderEvent{Kind: "tool_start", ID: id, Name: "shell_execute"}); err != nil {
			return ProviderResult{}, err
		}
		args, _ := json.Marshal(map[string]any{"command": cmd, "tool_title": "Mock shell command", "timeout": 30})
		if err := emit(ProviderEvent{Kind: "tool_input", ID: id, Name: "shell_execute", Args: string(args)}); err != nil {
			return ProviderResult{}, err
		}
		if err := emit(ProviderEvent{Kind: "tool_call", ID: id, Name: "shell_execute", Args: string(args)}); err != nil {
			return ProviderResult{}, err
		}
		return ProviderResult{StopReason: "tool_use", Usage: Usage{InputTokens: 12, OutputTokens: 10, LatestContextTokens: 12}}, nil
	}
	if name, ok := mockClientTool(last); ok {
		id := "call_mock_client"
		if err := emit(ProviderEvent{Kind: "tool_start", ID: id, Name: name}); err != nil {
			return ProviderResult{}, err
		}
		args := `{"tool_title":"Mock client tool"}`
		if err := emit(ProviderEvent{Kind: "tool_input", ID: id, Name: name, Args: args}); err != nil {
			return ProviderResult{}, err
		}
		if err := emit(ProviderEvent{Kind: "tool_call", ID: id, Name: name, Args: args}); err != nil {
			return ProviderResult{}, err
		}
		return ProviderResult{StopReason: "tool_use", Usage: Usage{InputTokens: 12, OutputTokens: 10, LatestContextTokens: 12}}, nil
	}
	reply := "OpenMinis server is connected. This is the offline **mock** provider.\n\n" +
		"You said: " + strings.TrimSpace(last) + "\n\n" +
		"Configure a real provider in `/data/config.json` to use a model. To test the server tool loop, send `[[shell:uname -a]]`."
	if err := streamText(ctx, emit, reply); err != nil {
		return ProviderResult{}, err
	}
	return ProviderResult{StopReason: "end_turn", Usage: Usage{InputTokens: 25, OutputTokens: len(reply) / 4, LatestContextTokens: 25}}, nil
}

func streamText(ctx context.Context, emit func(ProviderEvent) error, text string) error {
	for len(text) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n := 24
		if n > len(text) {
			n = len(text)
		}
		if err := emit(ProviderEvent{Kind: "text", Text: text[:n]}); err != nil {
			return err
		}
		text = text[n:]
	}
	return nil
}

func mockShellCommand(text string) (string, bool) {
	const prefix = "[[shell:"
	i := strings.Index(text, prefix)
	if i < 0 {
		return "", false
	}
	rest := text[i+len(prefix):]
	j := strings.Index(rest, "]]")
	if j < 0 {
		return "", false
	}
	cmd := strings.TrimSpace(rest[:j])
	return cmd, cmd != ""
}

func mockClientTool(text string) (string, bool) {
	const prefix = "[[client:"
	i := strings.Index(text, prefix)
	if i < 0 {
		return "", false
	}
	rest := text[i+len(prefix):]
	j := strings.Index(rest, "]]")
	if j < 0 {
		return "", false
	}
	name := strings.TrimSpace(rest[:j])
	return name, name != ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… (truncated)"
}
