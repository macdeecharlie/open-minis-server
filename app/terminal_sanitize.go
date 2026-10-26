package main

import (
	"regexp"
	"strings"
)

// Server port of the official TerminalSanitizer. Shell output is captured from
// a real PTY-less process, but commands still emit ANSI colours, carriage
// returns for progress lines, and control bytes. The model must see the same
// cleaned text the Android loop produces.
var (
	ansiSequence = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)
	nullRun      = regexp.MustCompile(`(?:null){2,}`)
	blankRuns    = regexp.MustCompile(`\n{3,}`)
)

func sanitizeTerminalOutput(raw string) string {
	if raw == "" {
		return raw
	}
	raw = truncateMiddle(raw, 1_000_000)
	raw = foldCarriageReturns(raw)
	raw = ansiSequence.ReplaceAllString(raw, "")
	var clean strings.Builder
	clean.Grow(len(raw))
	for _, r := range raw {
		if r == '\n' || r == '\t' || r >= 0x20 {
			clean.WriteRune(r)
		}
	}
	lines := strings.Split(clean.String(), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) == "null" {
			continue
		}
		kept = append(kept, line)
	}
	out := strings.Join(kept, "\n")
	out = nullRun.ReplaceAllString(out, "")
	out = blankRuns.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

// foldCarriageReturns applies terminal overwrite semantics per line: every \r
// moves the cursor to column 0 and the next segment overwrites rune by rune,
// leaving any longer previous tail visible. Empty segments are no-ops.
func foldCarriageReturns(raw string) string {
	if !strings.ContainsRune(raw, '\r') {
		return raw
	}
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		if !strings.ContainsRune(line, '\r') {
			continue
		}
		var out []rune
		col := 0
		for _, seg := range strings.Split(line, "\r") {
			if seg == "" {
				continue
			}
			for _, r := range seg {
				if col < len(out) {
					out[col] = r
				} else {
					out = append(out, r)
				}
				col++
			}
		}
		lines[i] = string(out)
	}
	return strings.Join(lines, "\n")
}

// truncateMiddle matches TerminalSanitizer.truncateIfNeeded: keep the head and
// tail, report how much was omitted.
func truncateMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	keep := max / 2
	omitted := len(r) - max
	return string(r[:keep]) + "\n\n[... " + itoa(omitted) + " characters omitted ...]\n\n" + string(r[len(r)-keep:])
}

// formatShellResult mirrors the official shell tool result: output alone,
// "(no output)" when empty, and an "(exit code: N)" suffix only on failure.
func formatShellResult(output string, exitCode int, timedOut bool) string {
	output = sanitizeTerminalOutput(output)
	output = truncateMiddle(output, 50_000)
	if strings.TrimSpace(output) == "" {
		output = "(no output)"
	}
	if timedOut {
		exitCode = 124
	}
	if exitCode != 0 {
		suffix := "(exit code: " + itoa(exitCode) + ")"
		if !strings.HasSuffix(strings.TrimRight(output, " \t\r\n"), suffix) {
			output += "\n" + suffix
		}
	}
	return output
}
