package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	offloadedPrefix     = "[CONTEXT OFFLOADED]"
	offloadNoticeTag    = "[Minis System Notice]"
	offloadReminderOpen = "<system-reminder>"
)

type offloadCandidate struct {
	msg       int
	part      int
	toolID    string
	toolName  string
	tokens    int
	bytes     int
	imageData string
	imageMime string
	arg       bool
}

// offloadPolicy mirrors ContextPolicy.forContextWindow for the offload tier.
func offloadThreshold(window int) int {
	switch {
	case window < 64_000:
		return 0
	case window < 128_000:
		return window - 20_000
	default:
		return window - 40_000
	}
}

func offloadTarget(window int) int {
	switch {
	case window < 64_000:
		return 0
	case window < 128_000:
		return window - 30_000
	default:
		return window - 60_000
	}
}

// offloadHistory is the server port of the official context offload pass:
// it writes older large tool results, tool arguments and images to
// /var/minis/offloads/tools and replaces them with the same stub text. The
// last four messages are protected. It runs only when the request has crossed
// the model's offload threshold.
func (s *Server) offloadHistory(sb ToolSandbox, messages []WireMessage, system string, tools []ToolDef, window int) ([]WireMessage, int) {
	threshold := offloadThreshold(window)
	if threshold <= 0 {
		return messages, 0
	}
	current := estimatedRequestTokens(system, messages, tools)
	if current < threshold {
		return messages, 0
	}
	target := offloadTarget(window)
	protected := 4
	if len(messages) < protected {
		protected = len(messages)
	}
	upper := len(messages) - protected

	out := make([]WireMessage, len(messages))
	for i := range messages {
		out[i] = messages[i]
		out[i].Parts = append([]WirePart(nil), messages[i].Parts...)
	}
	answered := map[string]bool{}
	for _, m := range out {
		for _, p := range m.Parts {
			if p.Type == "tool_result" {
				answered[p.ID] = true
			}
		}
	}
	var candidates []offloadCandidate
	for i := 0; i < upper; i++ {
		for j, p := range out[i].Parts {
			switch p.Type {
			case "tool_result":
				if strings.HasPrefix(strings.TrimSpace(p.Content), offloadedPrefix) {
					continue
				}
				contentLarge := len(p.Content) > 500
				imageLarge := len(p.Data) > 1024
				if !contentLarge && !imageLarge {
					continue
				}
				tokens := estimateTextTokens(p.Content)
				if imageLarge {
					tokens += estimateImageTokens(p.Data)
				}
				candidates = append(candidates, offloadCandidate{
					msg: i, part: j, toolID: p.ID, toolName: p.Name, tokens: tokens,
					bytes: len(p.Content) + len(p.Data)/4*3, imageData: p.Data, imageMime: p.Mime,
				})
			case "tool_use":
				if p.Name != "file_write" && p.Name != "file_edit" {
					continue
				}
				var args map[string]any
				if json.Unmarshal(p.Args, &args) != nil {
					continue
				}
				content, _ := args["content"].(string)
				if len(content) <= 500 || !answered[p.ID] {
					continue
				}
				candidates = append(candidates, offloadCandidate{
					msg: i, part: j, toolID: p.ID, toolName: p.Name,
					tokens: estimateTextTokens(content), bytes: len(content), arg: true,
				})
			case "image":
				if len(p.Data) <= 1024 {
					continue
				}
				candidates = append(candidates, offloadCandidate{
					msg: i, part: j, toolID: fmt.Sprintf("img%d_%d", i, j), toolName: "image",
					tokens: estimateImageTokens(p.Data), bytes: len(p.Data) / 4 * 3, imageData: p.Data, imageMime: p.Mime,
				})
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].tokens > candidates[j].tokens })

	count := 0
	for _, c := range candidates {
		if current <= target {
			break
		}
		path := ""
		if c.arg {
			var args map[string]any
			_ = json.Unmarshal(out[c.msg].Parts[c.part].Args, &args)
			content, _ := args["content"].(string)
			if content != "" {
				path = s.writeOffloadText(sb, c.toolName, c.toolID, content, "txt")
				if path != "" {
					args["content"] = offloadArgumentNotice(c.tokens, len(content), path)
					if b, err := json.Marshal(args); err == nil {
						out[c.msg].Parts[c.part].Args = b
					}
				}
			}
		} else if c.imageData != "" {
			ext := offloadImageExt(c.imageMime)
			raw, err := base64.StdEncoding.DecodeString(c.imageData)
			if err == nil {
				p := "/var/minis/offloads/tools/image_" + shortToolID(c.toolID) + "." + ext
				if err := sb.Upload(p, raw); err == nil {
					path = p
				}
			}
			if len(out[c.msg].Parts[c.part].Content) > 500 {
				if textPath := s.writeOffloadText(sb, c.toolName, c.toolID, out[c.msg].Parts[c.part].Content, "txt"); textPath != "" {
					path = textPath
				}
			}
			if path != "" {
				out[c.msg].Parts[c.part].Content = offloadStub(c.tokens, c.bytes, path)
			}
			out[c.msg].Parts[c.part].Data = ""
			out[c.msg].Parts[c.part].Mime = ""
		} else if out[c.msg].Parts[c.part].Content != "" {
			path = s.writeOffloadText(sb, c.toolName, c.toolID, out[c.msg].Parts[c.part].Content, "txt")
			if path != "" {
				out[c.msg].Parts[c.part].Content = offloadStub(c.tokens, len(out[c.msg].Parts[c.part].Content), path)
			}
		}
		if path == "" {
			continue
		}
		current -= c.tokens
		count++
	}
	return out, count
}

func (s *Server) writeOffloadText(sb ToolSandbox, toolName, toolID, content, ext string) string {
	if strings.TrimSpace(toolName) == "" {
		toolName = "tool"
	}
	name := strings.ReplaceAll(toolName, "/", "_") + "_" + shortToolID(toolID) + "." + ext
	path := "/var/minis/offloads/tools/" + name
	if err := sb.WriteFile(path, content, false, true); err != nil {
		return ""
	}
	return path
}

func shortToolID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[len(id)-12:]
}

func offloadImageExt(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return "bin"
	}
}

func offloadStub(tokens, bytes int, path string) string {
	return offloadedPrefix + " Content (~" + itoa(tokens) + " tokens, " + itoa(bytes) + " bytes) saved to: " + path + "\n" +
		"Use file_read tool to retrieve if needed."
}

func offloadArgumentNotice(tokens, bytes int, path string) string {
	return offloadReminderOpen + "\n" +
		offloadNoticeTag + " The original content (~" + itoa(tokens) + " tokens, " + itoa(bytes) +
		" bytes) of this tool argument has been pruned to save context and offloaded to: " + path + "\n\n" +
		"CRITICAL: This is a placeholder notice, NOT actual file content. NEVER pass this placeholder " +
		"or reuse it in subsequent file_write or file_edit calls. If you need the original content, " +
		"call file_read on the offloaded path first.\n</system-reminder>"
}
