// Package config loads Overmind configuration from ~/.overmind/config.yaml
// with OVERMIND_* environment variable overrides.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// LLM holds the legacy (v1) single-block planner LLM settings. Deprecated:
// declare `providers` + `roles` instead; a config that only has this block
// is auto-migrated to the "default" provider profile with a warning.
type LLM struct {
	Provider      string   `yaml:"provider"`        // anthropic | openai | cli (empty = auto-detect)
	Model         string   `yaml:"model"`
	APIKeyEnv     string   `yaml:"api_key_env"`     // env var holding the API key (empty = provider default)
	BaseURL       string   `yaml:"base_url"`        // openai provider: API base, e.g. https://api.deepseek.com
	CLICommand    string   `yaml:"cli_command"`     // cli provider: binary to run
	CLIArgs       []string `yaml:"cli_args"`        // cli provider: args before the prompt
	CLITimeoutSec int      `yaml:"cli_timeout_sec"` // cli provider: subprocess timeout
}

// Provider is one named LLM endpoint in the v2 `providers` map. Declare as
// many as you like (DeepSeek + Ollama + Groq...) and point roles at them.
type Provider struct {
	Type       string   `yaml:"type"`        // openai-compatible | anthropic | cli (empty = auto-detect)
	BaseURL    string   `yaml:"base_url"`    // openai-compatible: API base URL
	APIKeyEnv  string   `yaml:"api_key_env"` // env var NAME holding the API key (optional for local base_url)
	Command    string   `yaml:"command"`     // cli: binary to run
	Args       []string `yaml:"args"`        // cli: args before the prompt
	TimeoutSec int      `yaml:"timeout_sec"` // cli: subprocess timeout (seconds)
}

// Role assigns a named provider (and model) to one role (planner, verifier...).
type Role struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// Config is the full Overmind configuration.
type Config struct {
	AOBaseURL              string              `yaml:"ao_base_url"`
	LLM                    LLM                 `yaml:"llm"` // legacy v1 block (see LLM)
	Providers              map[string]Provider `yaml:"providers"`
	Roles                  map[string]Role     `yaml:"roles"`
	DBPath                 string              `yaml:"db_path"`
	MaxParallel            int                 `yaml:"max_parallel"`
	PollIntervalSec        int                 `yaml:"poll_interval_sec"`
	TaskTimeoutMin         int                 `yaml:"task_timeout_min"`
	NoSignalTimeoutMin     int                 `yaml:"no_signal_timeout_min"`
	IdleNoMarkerTimeoutMin int                 `yaml:"idle_no_marker_timeout_min"`
	MaxVerifyRounds        int                 `yaml:"max_verify_rounds"` // retry budget per task on verify fail (0 = no retries)

	// Warnings collected while loading (e.g. legacy-config migration notice).
	// Not part of the YAML schema; callers should surface them to the user.
	Warnings []string `yaml:"-"`
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
		MaxVerifyRounds:        2,
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
	migrate(&cfg, hasLegacyLLM(data))
	if err := validate(&cfg); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// hasLegacyLLM reports whether the user actively configured the legacy v1
// `llm` block, via the config file or an OVERMIND_LLM_* env var.
func hasLegacyLLM(data []byte) bool {
	var probe struct {
		LLM *LLM `yaml:"llm"`
	}
	if yaml.Unmarshal(data, &probe) == nil && probe.LLM != nil {
		return true
	}
	for _, key := range []string{
		"OVERMIND_LLM_PROVIDER", "OVERMIND_LLM_MODEL", "OVERMIND_LLM_API_KEY_ENV",
		"OVERMIND_LLM_BASE_URL", "OVERMIND_LLM_CLI_COMMAND", "OVERMIND_LLM_CLI_TIMEOUT_SEC",
	} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// migrate maps a legacy v1 config (single `llm` block) onto the v2 schema:
// the block becomes the "default" provider profile and roles.planner points
// at it. legacy indicates the user actively set v1 fields (warning-worthy).
func migrate(cfg *Config, legacy bool) {
	if len(cfg.Providers) > 0 {
		if legacy {
			cfg.Warnings = append(cfg.Warnings,
				"both `providers` and the legacy `llm` block are set; the `llm` block is ignored — remove it")
		}
		return
	}
	cfg.Providers = map[string]Provider{"default": {
		Type:       legacyProviderType(cfg.LLM.Provider),
		BaseURL:    cfg.LLM.BaseURL,
		APIKeyEnv:  cfg.LLM.APIKeyEnv,
		Command:    cfg.LLM.CLICommand,
		Args:       cfg.LLM.CLIArgs,
		TimeoutSec: cfg.LLM.CLITimeoutSec,
	}}
	if cfg.Roles == nil {
		cfg.Roles = map[string]Role{}
	}
	if _, ok := cfg.Roles["planner"]; !ok {
		cfg.Roles["planner"] = Role{Provider: "default", Model: cfg.LLM.Model}
	}
	if legacy {
		cfg.Warnings = append(cfg.Warnings,
			"config uses the legacy `llm` block (mapped to providers.default); migrate to `providers` + `roles` — see config.example.yaml")
	}
}

// legacyProviderType maps a v1 llm.provider value to a v2 provider type.
func legacyProviderType(p string) string {
	if strings.EqualFold(strings.TrimSpace(p), "openai") {
		return "openai-compatible"
	}
	return strings.ToLower(strings.TrimSpace(p))
}

// validate checks the v2 provider/role schema (runs after migration, so a
// legacy config is validated in its migrated form).
func validate(cfg *Config) error {
	if cfg.MaxVerifyRounds < 0 {
		return fmt.Errorf("max_verify_rounds must be >= 0, got %d", cfg.MaxVerifyRounds)
	}
	for name, p := range cfg.Providers {
		switch strings.ToLower(strings.TrimSpace(p.Type)) {
		case "", "auto", "anthropic", "cli", "openai", "openai-compatible":
		default:
			return fmt.Errorf("providers.%s: unknown type %q (expected openai-compatible, anthropic or cli)", name, p.Type)
		}
		if strings.EqualFold(strings.TrimSpace(p.Type), "cli") && p.Command == "" {
			return fmt.Errorf("providers.%s: command is required for type cli", name)
		}
	}
	if _, ok := cfg.Roles["planner"]; !ok {
		return fmt.Errorf("roles.planner is required (assign it a provider + model)")
	}
	for role, r := range cfg.Roles {
		if _, ok := cfg.Providers[r.Provider]; !ok {
			return fmt.Errorf("roles.%s: provider %q is not declared under providers", role, r.Provider)
		}
	}
	return nil
}

// LLMForRole resolves a role name (planner, verifier...) to its role
// assignment and the named provider it points at.
func (c Config) LLMForRole(role string) (Role, Provider, error) {
	r, ok := c.Roles[role]
	if !ok {
		return Role{}, Provider{}, fmt.Errorf("no roles.%s in config (assign it a provider + model)", role)
	}
	p, ok := c.Providers[r.Provider]
	if !ok {
		return Role{}, Provider{}, fmt.Errorf("roles.%s: provider %q is not declared under providers", role, r.Provider)
	}
	return r, p, nil
}

// IsLocalBaseURL reports whether baseURL points at this machine (localhost,
// loopback or unspecified IP). Local OpenAI-compatible endpoints like Ollama
// do not require an API key.
func IsLocalBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
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
	envInt("OVERMIND_MAX_VERIFY_ROUNDS", &cfg.MaxVerifyRounds)
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
