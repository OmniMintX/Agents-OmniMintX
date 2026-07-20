// Package config loads Overmind configuration from ~/.overmind/config.yaml
// with OVERMIND_* environment variable overrides.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// LLM holds planner LLM settings.
type LLM struct {
	Provider      string   `yaml:"provider"`        // anthropic | openai | cli (empty = auto-detect)
	Model         string   `yaml:"model"`
	APIKeyEnv     string   `yaml:"api_key_env"`     // env var holding the API key (empty = provider default)
	BaseURL       string   `yaml:"base_url"`        // openai provider: API base, e.g. https://api.deepseek.com
	CLICommand    string   `yaml:"cli_command"`     // cli provider: binary to run
	CLIArgs       []string `yaml:"cli_args"`        // cli provider: args before the prompt
	CLITimeoutSec int      `yaml:"cli_timeout_sec"` // cli provider: subprocess timeout
}

// Config is the full Overmind configuration.
type Config struct {
	AOBaseURL              string `yaml:"ao_base_url"`
	LLM                    LLM    `yaml:"llm"`
	DBPath                 string `yaml:"db_path"`
	MaxParallel            int    `yaml:"max_parallel"`
	PollIntervalSec        int    `yaml:"poll_interval_sec"`
	TaskTimeoutMin         int    `yaml:"task_timeout_min"`
	NoSignalTimeoutMin     int    `yaml:"no_signal_timeout_min"`
	IdleNoMarkerTimeoutMin int    `yaml:"idle_no_marker_timeout_min"`
}

// Dir returns the Overmind config directory (~/.overmind).
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".overmind"), nil
}

// DefaultPath returns the default config file path (~/.overmind/config.yaml).
func DefaultPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Default returns a Config populated with default values.
func Default() (Config, error) {
	dir, err := Dir()
	if err != nil {
		return Config{}, err
	}
	return Config{
		AOBaseURL: "http://127.0.0.1:3001",
		LLM: LLM{
			Provider:      "", // auto-detect: cli when the binary exists, else anthropic
			Model:         "claude-sonnet-4-5",
			APIKeyEnv:     "", // resolved per provider (ANTHROPIC_API_KEY / OPENAI_API_KEY)
			CLICommand:    "claude",
			CLIArgs:       []string{"-p", "--output-format", "json"},
			CLITimeoutSec: 180,
		},
		DBPath:                 filepath.Join(dir, "overmind.db"),
		MaxParallel:            3,
		PollIntervalSec:        15,
		TaskTimeoutMin:         45,
		NoSignalTimeoutMin:     10,
		IdleNoMarkerTimeoutMin: 10,
	}, nil
}

// Load reads config from path (default: ~/.overmind/config.yaml when empty),
// applies OVERMIND_* env overrides, and falls back to defaults for unset fields.
// A missing config file is not an error.
func Load(path string) (Config, error) {
	cfg, err := Default()
	if err != nil {
		return Config{}, err
	}
	if path == "" {
		path, err = DefaultPath()
		if err != nil {
			return Config{}, err
		}
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	applyEnv(&cfg)
	return cfg, nil
}

func applyEnv(cfg *Config) {
	envStr("OVERMIND_AO_BASE_URL", &cfg.AOBaseURL)
	envStr("OVERMIND_LLM_PROVIDER", &cfg.LLM.Provider)
	envStr("OVERMIND_LLM_MODEL", &cfg.LLM.Model)
	envStr("OVERMIND_LLM_API_KEY_ENV", &cfg.LLM.APIKeyEnv)
	envStr("OVERMIND_LLM_BASE_URL", &cfg.LLM.BaseURL)
	envStr("OVERMIND_LLM_CLI_COMMAND", &cfg.LLM.CLICommand)
	envInt("OVERMIND_LLM_CLI_TIMEOUT_SEC", &cfg.LLM.CLITimeoutSec)
	envStr("OVERMIND_DB_PATH", &cfg.DBPath)
	envInt("OVERMIND_MAX_PARALLEL", &cfg.MaxParallel)
	envInt("OVERMIND_POLL_INTERVAL_SEC", &cfg.PollIntervalSec)
	envInt("OVERMIND_TASK_TIMEOUT_MIN", &cfg.TaskTimeoutMin)
	envInt("OVERMIND_NO_SIGNAL_TIMEOUT_MIN", &cfg.NoSignalTimeoutMin)
	envInt("OVERMIND_IDLE_NO_MARKER_TIMEOUT_MIN", &cfg.IdleNoMarkerTimeoutMin)
}

func envStr(key string, dst *string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func envInt(key string, dst *int) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}
