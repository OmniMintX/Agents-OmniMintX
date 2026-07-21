package verifier

import (
	"strings"
	"testing"
)

const failFixture = `verdict: fail
reason: the handler is missing
feedback:
- file: api/server.go
  issue: no /health route registered
  suggestion: add a GET /health handler returning 200
- issue: main does not wire the router
  suggestion: call api.NewRouter() from main
`

func TestParseCleanOK(t *testing.T) {
	v, err := Parse([]byte("verdict: ok\nreason: task done\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if v.Verdict != VerdictOK || v.Reason != "task done" || len(v.Feedback) != 0 {
		t.Fatalf("unexpected verdict: %+v", v)
	}
}

func TestParseCleanFail(t *testing.T) {
	v, err := Parse([]byte(failFixture))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if v.Verdict != VerdictFail || len(v.Feedback) != 2 {
		t.Fatalf("unexpected verdict: %+v", v)
	}
	if v.Feedback[0].File != "api/server.go" || v.Feedback[0].Issue == "" || v.Feedback[0].Suggestion == "" {
		t.Fatalf("unexpected item[0]: %+v", v.Feedback[0])
	}
	if v.Feedback[1].File != "" {
		t.Fatalf("item[1].File should be empty: %+v", v.Feedback[1])
	}
}

func TestParseFenced(t *testing.T) {
	for name, raw := range map[string]string{
		"yaml fence": "```yaml\n" + failFixture + "```\n",
		"bare fence": "```\n" + failFixture + "```",
		"with prose": "Here is my grading:\n```yaml\n" + failFixture + "```\nHope that helps!",
	} {
		v, err := Parse([]byte(raw))
		if err != nil {
			t.Errorf("%s: Parse: %v", name, err)
			continue
		}
		if v.Verdict != VerdictFail || len(v.Feedback) != 2 {
			t.Errorf("%s: unexpected verdict: %+v", name, v)
		}
	}
}

func TestParseProseFallback(t *testing.T) {
	raw := "Sure! After reviewing the diff carefully, my verdict is below.\n\n" + failFixture
	v, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if v.Verdict != VerdictFail || len(v.Feedback) != 2 {
		t.Fatalf("unexpected verdict: %+v", v)
	}
}

func TestParseMalformed(t *testing.T) {
	cases := map[string]string{
		"garbage":           "total garbage, no yaml at all",
		"broken yaml":       "verdict: [unclosed\nreason: x",
		"empty":             "",
		"unknown verdict":   "verdict: maybe\nreason: not sure\n",
		"fail empty reason": "verdict: fail\nreason: \"\"\n",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestParseNormalizesVerdict(t *testing.T) {
	v, err := Parse([]byte("verdict: \"  OK \"\nreason: fine\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if v.Verdict != VerdictOK {
		t.Fatalf("want normalized %q, got %q", VerdictOK, v.Verdict)
	}
}

func TestParseCapsFeedback(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("verdict: fail\nreason: many problems\nfeedback:\n")
	for i := 0; i < maxFeedbackItems+3; i++ {
		sb.WriteString("- issue: problem\n  suggestion: fix it\n")
	}
	v, err := Parse([]byte(sb.String()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(v.Feedback) != maxFeedbackItems {
		t.Fatalf("want feedback capped at %d, got %d", maxFeedbackItems, len(v.Feedback))
	}
}
