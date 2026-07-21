package verifier

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse decodes and validates a raw LLM verdict response. It tolerates
// sloppy output (pr-agent style): markdown fences are stripped first; if
// decoding or validation still fails, it retries on the slice from the
// first line starting with "verdict:" to the end (dropping leading prose).
func Parse(data []byte) (Verdict, error) {
	text := stripFences(strings.TrimSpace(string(data)))
	v, err := parseAndValidate(text)
	if err == nil {
		return v, nil
	}
	if cut, ok := cutFromVerdict(text); ok {
		if v2, err2 := parseAndValidate(cut); err2 == nil {
			return v2, nil
		}
	}
	return Verdict{}, err
}

// parseAndValidate is one strict decode+validate attempt on cleaned text.
func parseAndValidate(text string) (Verdict, error) {
	var v Verdict
	if err := yaml.Unmarshal([]byte(text), &v); err != nil {
		return Verdict{}, fmt.Errorf("verdict YAML does not match the schema: %w", err)
	}
	if err := validate(&v); err != nil {
		return Verdict{}, err
	}
	return v, nil
}

// validate normalizes and enforces the schema rules: verdict must be
// ok|fail, a fail verdict needs a non-empty reason, and feedback is
// silently capped at maxFeedbackItems.
func validate(v *Verdict) error {
	v.Verdict = strings.ToLower(strings.TrimSpace(v.Verdict))
	v.Reason = strings.TrimSpace(v.Reason)
	if v.Verdict != VerdictOK && v.Verdict != VerdictFail {
		return fmt.Errorf("verdict must be %q or %q, got %q", VerdictOK, VerdictFail, v.Verdict)
	}
	if v.Verdict == VerdictFail && v.Reason == "" {
		return fmt.Errorf("a %q verdict requires a non-empty reason", VerdictFail)
	}
	if len(v.Feedback) > maxFeedbackItems {
		v.Feedback = v.Feedback[:maxFeedbackItems]
	}
	return nil
}

// stripFences extracts the content of a ```yaml (or bare ```) fenced block,
// or returns the input unchanged when no fence is present.
func stripFences(s string) string {
	for _, fence := range []string{"```yaml", "```"} {
		i := strings.Index(s, fence)
		if i < 0 {
			continue
		}
		rest := s[i+len(fence):]
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		return strings.TrimSpace(rest)
	}
	return s
}

// cutFromVerdict slices from the first line starting with "verdict:" to
// the end, dropping any leading prose that breaks YAML decoding.
func cutFromVerdict(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "verdict:") {
			if i == 0 {
				return "", false // nothing to cut; avoid re-parsing the same text
			}
			return strings.Join(lines[i:], "\n"), true
		}
	}
	return "", false
}
