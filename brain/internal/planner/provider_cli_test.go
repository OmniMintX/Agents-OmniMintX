package planner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// All CLI tests run against fake shell scripts — never a real agent CLI.

// fakeCLI writes an executable /bin/sh script and returns its path.
func fakeCLI(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Real `claude -p --output-format json` envelope shape (research note): one
// JSON object, the response text in .result — here with prose and a fenced
// JSON block around the plan.
func TestCLIClaudeEnvelope(t *testing.T) {
	cmd := fakeCLI(t, `cat <<'EOF'
{"type":"result","subtype":"success","is_error":false,"duration_ms":9000,"num_turns":1,"result":"Here is the plan:\n`+"```"+`json\n{\"tasks\":[{\"title\":\"a\"}]}\n`+"```"+`\nDone.","session_id":"abc"}
EOF`)
	out, err := NewCLI(cmd, nil, time.Minute).Complete(context.Background(), "plan it")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != `{"tasks":[{"title":"a"}]}` {
		t.Fatalf("want first JSON block from .result, got %q", out)
	}
}

func TestCLIClaudeErrorEnvelope(t *testing.T) {
	cmd := fakeCLI(t, `printf '%s' '{"type":"result","subtype":"error","is_error":true,"result":"credit balance too low"}'`)
	_, err := NewCLI(cmd, nil, time.Minute).Complete(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "credit balance too low") {
		t.Fatalf("want agent error surfaced, got %v", err)
	}
}

// A non-claude CLI printing prose around JSON (with stray braces after it)
// must still yield the first balanced JSON object from raw stdout.
func TestCLIGenericOutputWithGarbage(t *testing.T) {
	cmd := fakeCLI(t, `printf '%s\n' 'INFO starting up...' '{"tasks":[{"title":"b {x}"}]} trailing } junk }'`)
	out, err := NewCLI(cmd, nil, time.Minute).Complete(context.Background(), "p")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != `{"tasks":[{"title":"b {x}"}]}` {
		t.Fatalf("want first balanced JSON object, got %q", out)
	}
}

// opencode-style JSONL: text lives in part.text of type=="text" events.
func TestCLIOpencodeJSONL(t *testing.T) {
	cmd := fakeCLI(t, `cat <<'EOF'
{"type":"step_start","part":{}}
{"type":"text","part":{"text":"{\"tasks\":[]}"}}
{"type":"step_finish","part":{}}
EOF`)
	out, err := NewCLI(cmd, nil, time.Minute).Complete(context.Background(), "p")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != `{"tasks":[]}` {
		t.Fatalf("want text-event payload, got %q", out)
	}
}

// Output without any JSON object is returned as-is; Generate's retry loop
// (above the LLM interface) handles the parse failure.
func TestCLINoJSONFallsThrough(t *testing.T) {
	cmd := fakeCLI(t, `printf 'no json here'`)
	out, err := NewCLI(cmd, nil, time.Minute).Complete(context.Background(), "p")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "no json here" {
		t.Fatalf("got %q", out)
	}
}

// Configured args must precede the prompt, which is appended last.
func TestCLIPromptIsLastArg(t *testing.T) {
	cmd := fakeCLI(t, `[ "$1" = "-p" ] || exit 9
for a; do last="$a"; done
printf '{"echo":"%s"}' "$last"`)
	out, err := NewCLI(cmd, []string{"-p", "--output-format", "json"}, time.Minute).Complete(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != `{"echo":"hello"}` {
		t.Fatalf("prompt not passed as last arg, got %q", out)
	}
}

func TestCLIMissingBinary(t *testing.T) {
	_, err := NewCLI("om-test-no-such-binary-xyz", nil, time.Second).Complete(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "not found in PATH") {
		t.Fatalf("want clear missing-binary error, got %v", err)
	}
}

func TestCLITimeout(t *testing.T) {
	cmd := fakeCLI(t, `sleep 5`)
	_, err := NewCLI(cmd, nil, 100*time.Millisecond).Complete(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
}

func TestCLIExitError(t *testing.T) {
	cmd := fakeCLI(t, `echo "boom: bad flag" >&2
exit 3`)
	_, err := NewCLI(cmd, nil, time.Minute).Complete(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "boom: bad flag") {
		t.Fatalf("want stderr surfaced in error, got %v", err)
	}
}
