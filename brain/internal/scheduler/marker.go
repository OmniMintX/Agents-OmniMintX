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

// maxSpawnPrompt is AO's hard cap on spawn prompt size (bytes).
const maxSpawnPrompt = 4096

// feedbackHeader introduces the verifier feedback block on a re-dispatch.
const feedbackHeader = "\n\n--- VERIFIER FEEDBACK (previous attempt was rejected; fix these) ---\n"

// minFeedbackBudget is the smallest byte budget worth spending on the full
// feedback block; below it the block would be useless noise, so the prompt
// falls back to a one-line "verification failed: <reason>" instead.
const minFeedbackBudget = 100

// feedbackFallback prefixes the minimal retry notice used when the full
// feedback block does not fit.
const feedbackFallback = "\n\nverification failed: "

// promptWithFeedbackAndFooter builds the spawn prompt for a (re-)dispatch:
// task prompt + optional verifier feedback + the marker protocol footer.
// The feedback is truncated so the whole prompt stays within AO's
// 4096-byte cap — the footer is never truncated (the marker protocol must
// survive intact). When fewer than minFeedbackBudget bytes remain for the
// feedback block, a minimal "verification failed: <reason>" line (reason =
// first line of the feedback, truncated) is used so a retry is never
// dispatched without any hint of why.
func promptWithFeedbackAndFooter(prompt, feedback, markerPath string) string {
	footer := fmt.Sprintf(promptFooterFmt, markerPath)
	if feedback != "" {
		budget := maxSpawnPrompt - len(prompt) - len(footer) - len(feedbackHeader)
		if budget >= minFeedbackBudget {
			prompt += feedbackHeader + truncate(feedback, budget)
		} else if fb := maxSpawnPrompt - len(prompt) - len(footer) - len(feedbackFallback); fb > 0 {
			reason, _, _ := strings.Cut(feedback, "\n")
			prompt += feedbackFallback + truncate(reason, fb)
		}
	}
	return prompt + footer
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
