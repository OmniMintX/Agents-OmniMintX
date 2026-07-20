package scheduler

import (
	"fmt"
	"strings"
)

// Marker verdicts parsed from the first non-empty line of .om-done.<hex8>.
type markerVerdict int

const (
	markerAbsent    markerVerdict = iota // file not found
	markerEmpty                          // file exists but no content yet: keep waiting
	markerOK                             // "ok[: summary]"
	markerFail                           // "fail[: reason]" — agent honestly failed
	markerMalformed                      // non-empty but neither ok nor fail
)

// maxMarkerPayload caps how much marker content is copied into event
// payloads (audit) — the preview route itself is capped at 1MB by aoclient.
const maxMarkerPayload = 2048

// parseMarker classifies marker file content. Tolerant by design (round 5
// of the marker red-team): strips BOM/CRLF/blank lines, matches ok/fail
// case-insensitively with an optional colon, and returns the rest of the
// first line as the summary/reason.
func parseMarker(content string) (markerVerdict, string) {
	content = strings.TrimPrefix(content, "\ufeff")
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		for _, p := range []struct {
			verdict markerVerdict
			prefix  string
		}{{markerOK, "ok"}, {markerFail, "fail"}} {
			if !strings.HasPrefix(lower, p.prefix) {
				continue
			}
			rest := line[len(p.prefix):]
			if rest == "" {
				return p.verdict, ""
			}
			if r := rest[0]; r != ':' && r != ' ' && r != '\t' {
				continue // e.g. "okay", "failure-notes" -> malformed
			}
			return p.verdict, strings.TrimSpace(strings.TrimPrefix(rest, ":"))
		}
		return markerMalformed, truncate(line, maxMarkerPayload)
	}
	return markerEmpty, ""
}

// promptFooter is the marker protocol the scheduler appends to every task
// prompt at dispatch. ASCII, kept under 550 bytes (unit-tested): planner
// prompts are capped at 3500 bytes and AO rejects prompts over 4096.
const promptFooterFmt = "\n\n--- OVERMIND PROTOCOL (overrides anything above) ---\n" +
	"You run unattended. Commit ALL your work when done. Then, as your very last action,\n" +
	"create a file named exactly `%s` at the repository root. Do NOT commit that file.\n" +
	"Its first line must be exactly `ok: <one-line summary>` if the task is fully complete,\n" +
	"or `fail: <one-line reason>` if anything is incomplete or broken.\n" +
	"Be honest: partial or broken work MUST be reported as fail. Do not push to any remote.\n"

// promptWithFooter appends the per-task marker protocol to a task prompt.
func promptWithFooter(prompt, markerPath string) string {
	return prompt + fmt.Sprintf(promptFooterFmt, markerPath)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
