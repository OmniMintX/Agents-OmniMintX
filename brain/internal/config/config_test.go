package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AOBaseURL != "http://127.0.0.1:3001" {
		t.Errorf("AOBaseURL = %q", cfg.AOBaseURL)
	}
	if cfg.LLM.Provider != "" || cfg.LLM.Model != "claude-sonnet-4-5" || cfg.LLM.APIKeyEnv != "" {
		t.Errorf("LLM = %+v", cfg.LLM)
	}
	if cfg.LLM.CLICommand != "claude" || cfg.LLM.CLITimeoutSec != 180 {
		t.Errorf("LLM cli defaults = %+v", cfg.LLM)
	}
	if len(cfg.LLM.CLIArgs) != 3 || cfg.LLM.CLIArgs[0] != "-p" {
		t.Errorf("LLM.CLIArgs = %v", cfg.LLM.CLIArgs)
	}
	if filepath.Base(cfg.DBPath) != "overmind.db" {
		t.Errorf("DBPath = %q", cfg.DBPath)
	}
	if cfg.MaxParallel != 3 || cfg.PollIntervalSec != 15 || cfg.TaskTimeoutMin != 45 || cfg.NoSignalTimeoutMin != 10 {
		t.Errorf("numeric defaults = %+v", cfg)
	}
	if cfg.MaxVerifyRounds != 2 {
		t.Errorf("MaxVerifyRounds default = %d, want 2", cfg.MaxVerifyRounds)
	}
	p, ok := cfg.Providers["default"]
	if !ok {
		t.Fatalf("defaults should migrate to providers.default, got %+v", cfg.Providers)
	}
	if p.Type != "" || p.Command != "claude" || p.TimeoutSec != 180 {
		t.Errorf("providers.default = %+v", p)
	}
	r, ok := cfg.Roles["planner"]
	if !ok || r.Provider != "default" || r.Model != "claude-sonnet-4-5" {
		t.Errorf("roles.planner = %+v (ok=%v)", r, ok)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("pure defaults must not warn: %v", cfg.Warnings)
	}
}

func TestLoadFileAndEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("ao_base_url: http://127.0.0.1:9999\nllm:\n  model: from-file\nmax_parallel: 5\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OVERMIND_LLM_MODEL", "from-env")
	t.Setenv("OVERMIND_POLL_INTERVAL_SEC", "7")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AOBaseURL != "http://127.0.0.1:9999" {
		t.Errorf("file override AOBaseURL = %q", cfg.AOBaseURL)
	}
	if cfg.MaxParallel != 5 {
		t.Errorf("file override MaxParallel = %d", cfg.MaxParallel)
	}
	if cfg.LLM.Model != "from-env" {
		t.Errorf("env should beat file: LLM.Model = %q", cfg.LLM.Model)
	}
	if cfg.PollIntervalSec != 7 {
		t.Errorf("env override PollIntervalSec = %d", cfg.PollIntervalSec)
	}
	if cfg.LLM.CLICommand != "claude" {
		t.Errorf("unset field should keep default: %q", cfg.LLM.CLICommand)
	}
	if r := cfg.Roles["planner"]; r.Provider != "default" || r.Model != "from-env" {
		t.Errorf("migrated roles.planner should carry the env model: %+v", r)
	}
	if len(cfg.Warnings) == 0 {
		t.Error("legacy llm block should produce a migration warning")
	}
}

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigrateLegacyLLM(t *testing.T) {
	path := writeConfig(t, `
llm:
  provider: openai
  model: deepseek-chat
  base_url: https://api.deepseek.com
  api_key_env: DEEPSEEK_API_KEY
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := cfg.Providers["default"]
	if !ok {
		t.Fatalf("legacy llm should map to providers.default, got %+v", cfg.Providers)
	}
	if p.Type != "openai-compatible" || p.BaseURL != "https://api.deepseek.com" || p.APIKeyEnv != "DEEPSEEK_API_KEY" {
		t.Errorf("providers.default = %+v", p)
	}
	r := cfg.Roles["planner"]
	if r.Provider != "default" || r.Model != "deepseek-chat" {
		t.Errorf("roles.planner = %+v", r)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "migrate") {
		t.Errorf("want one migration warning, got %v", cfg.Warnings)
	}
}

func TestLoadV2Providers(t *testing.T) {
	path := writeConfig(t, `
providers:
  deepseek:
    type: openai-compatible
    base_url: https://api.deepseek.com
    api_key_env: DEEPSEEK_API_KEY
  ollama:
    type: openai-compatible
    base_url: http://localhost:11434/v1
  claude-cli:
    type: cli
    command: claude
    args: ["-p", "--output-format", "json"]
    timeout_sec: 120
roles:
  planner: { provider: deepseek, model: deepseek-chat }
  verifier: { provider: claude-cli, model: "" }
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Providers) != 3 {
		t.Fatalf("Providers = %+v", cfg.Providers)
	}
	if cfg.Providers["claude-cli"].TimeoutSec != 120 || cfg.Providers["claude-cli"].Args[0] != "-p" {
		t.Errorf("claude-cli = %+v", cfg.Providers["claude-cli"])
	}
	r, p, err := cfg.LLMForRole("planner")
	if err != nil || r.Model != "deepseek-chat" || p.Type != "openai-compatible" {
		t.Errorf("LLMForRole(planner) = %+v %+v %v", r, p, err)
	}
	if _, p, err := cfg.LLMForRole("verifier"); err != nil || p.Command != "claude" {
		t.Errorf("LLMForRole(verifier) = %+v %v", p, err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("pure v2 config must not warn: %v", cfg.Warnings)
	}
	if _, _, err := cfg.LLMForRole("reviewer"); err == nil {
		t.Error("unknown role should error")
	}
}

func TestV2WithLegacyBlockWarns(t *testing.T) {
	path := writeConfig(t, `
llm:
  model: old-model
providers:
  ollama:
    type: openai-compatible
    base_url: http://localhost:11434/v1
roles:
  planner: { provider: ollama, model: llama3.1 }
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "ignored") {
		t.Errorf("want ignored-llm warning, got %v", cfg.Warnings)
	}
	if r := cfg.Roles["planner"]; r.Model != "llama3.1" {
		t.Errorf("v2 roles must win over legacy llm: %+v", r)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name, yaml, wantErr string
	}{
		{"unknown provider type", `
providers:
  bad: { type: banana }
roles:
  planner: { provider: bad, model: m }
`, "unknown type"},
		{"cli without command", `
providers:
  c: { type: cli }
roles:
  planner: { provider: c, model: m }
`, "command is required"},
		{"role points at undeclared provider", `
providers:
  ok: { type: anthropic }
roles:
  planner: { provider: nope, model: m }
`, "not declared"},
		{"missing planner role", `
providers:
  ok: { type: anthropic }
roles:
  verifier: { provider: ok, model: m }
`, "roles.planner is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestMaxVerifyRounds(t *testing.T) {
	cfg, err := Load(writeConfig(t, "max_verify_rounds: 0\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxVerifyRounds != 0 {
		t.Errorf("file MaxVerifyRounds = %d, want 0", cfg.MaxVerifyRounds)
	}

	t.Setenv("OVERMIND_MAX_VERIFY_ROUNDS", "5")
	cfg, err = Load(writeConfig(t, "max_verify_rounds: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxVerifyRounds != 5 {
		t.Errorf("env should beat file: MaxVerifyRounds = %d, want 5", cfg.MaxVerifyRounds)
	}
	t.Setenv("OVERMIND_MAX_VERIFY_ROUNDS", "")

	if _, err := Load(writeConfig(t, "max_verify_rounds: -1\n")); err == nil || !strings.Contains(err.Error(), "max_verify_rounds") {
		t.Fatalf("negative max_verify_rounds should be rejected, got %v", err)
	}
}

func TestIsLocalBaseURL(t *testing.T) {
	local := []string{
		"http://localhost:11434/v1", "http://127.0.0.1:8080", "http://[::1]:1234/v1", "http://0.0.0.0:80",
	}
	for _, u := range local {
		if !IsLocalBaseURL(u) {
			t.Errorf("IsLocalBaseURL(%q) = false, want true", u)
		}
	}
	remote := []string{
		"https://api.deepseek.com", "https://api.openai.com/v1", "", "http://192.168.1.10:11434", "not a url",
	}
	for _, u := range remote {
		if IsLocalBaseURL(u) {
			t.Errorf("IsLocalBaseURL(%q) = true, want false", u)
		}
	}
}

func TestAutonomyValidValues(t *testing.T) {
	for _, v := range []string{AutonomyAuto, AutonomyAcceptEdits, AutonomyBypass, AutonomyOff} {
		cfg, err := Load(writeConfig(t, "autonomy: "+v+"\n"))
		if err != nil {
			t.Fatalf("Load(autonomy=%s): %v", v, err)
		}
		if cfg.Autonomy != v {
			t.Errorf("Autonomy = %q, want %q", cfg.Autonomy, v)
		}
		if cfg.AutonomyAllowBypass {
			t.Errorf("AutonomyAllowBypass must default to false")
		}
	}
}

func TestAutonomyEmptyDefaultsToAuto(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Autonomy != AutonomyAuto {
		t.Errorf("default Autonomy = %q, want %q", cfg.Autonomy, AutonomyAuto)
	}
	cfg, err = Load(writeConfig(t, "autonomy: \"\"\n"))
	if err != nil {
		t.Fatalf("Load(autonomy empty): %v", err)
	}
	if cfg.Autonomy != AutonomyAuto {
		t.Errorf("empty Autonomy = %q, want %q", cfg.Autonomy, AutonomyAuto)
	}
}

func TestAutonomyUnknownRejected(t *testing.T) {
	_, err := Load(writeConfig(t, "autonomy: yolo\n"))
	if err == nil || !strings.Contains(err.Error(), "autonomy") {
		t.Fatalf("Load(autonomy=yolo) err = %v, want unknown-autonomy error", err)
	}
}

func TestAutonomyAllowBypassFromFile(t *testing.T) {
	cfg, err := Load(writeConfig(t, "autonomy: bypass-permissions\nautonomy_allow_bypass: true\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Autonomy != AutonomyBypass || !cfg.AutonomyAllowBypass {
		t.Errorf("cfg = autonomy %q allowBypass %v", cfg.Autonomy, cfg.AutonomyAllowBypass)
	}
}

func TestNotifyValidValues(t *testing.T) {
	for _, v := range []string{NotifyAuto, NotifyBell, NotifyOff} {
		cfg, err := Load(writeConfig(t, "notify: "+v+"\n"))
		if err != nil {
			t.Fatalf("Load(notify=%s): %v", v, err)
		}
		if cfg.Notify != v {
			t.Errorf("Notify = %q, want %q", cfg.Notify, v)
		}
	}
}

func TestNotifyEmptyDefaultsToAuto(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Notify != NotifyAuto {
		t.Errorf("default Notify = %q, want %q", cfg.Notify, NotifyAuto)
	}
	cfg, err = Load(writeConfig(t, "notify: \"\"\n"))
	if err != nil {
		t.Fatalf("Load(notify empty): %v", err)
	}
	if cfg.Notify != NotifyAuto {
		t.Errorf("empty Notify = %q, want %q", cfg.Notify, NotifyAuto)
	}
}

func TestNotifyUnknownRejected(t *testing.T) {
	_, err := Load(writeConfig(t, "notify: loud\n"))
	if err == nil || !strings.Contains(err.Error(), "notify") {
		t.Fatalf("Load(notify=loud) err = %v, want unknown-notify error", err)
	}
}

func TestNotifyEnvOverride(t *testing.T) {
	t.Setenv("OVERMIND_NOTIFY", "off")
	cfg, err := Load(writeConfig(t, "notify: bell\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Notify != NotifyOff {
		t.Errorf("Notify = %q, want off (env wins over file)", cfg.Notify)
	}
}
