package config

import (
	"os"
	"path/filepath"
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
}
