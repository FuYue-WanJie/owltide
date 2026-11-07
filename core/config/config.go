// Package config loads ~/.owltide/config.yaml.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/FuYue-WanJie/owltide/core/platform"
)

// Model describes one OpenAI-compatible endpoint.
type Model struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
	// APIKeyEnv names an environment variable holding the key;
	// keys never live in the config file itself.
	APIKeyEnv string `yaml:"api_key_env"`
}

// Budget caps a single task run.
type Budget struct {
	// MaxTurns caps agent loop iterations (0 = unlimited).
	MaxTurns int `yaml:"max_turns"`
	// MaxTokens caps total tokens across a run (0 = unlimited).
	MaxTokens int `yaml:"max_tokens"`
	// ToolTimeoutSec caps one run_shell call (0 = 60s default).
	ToolTimeoutSec int `yaml:"tool_timeout_sec"`
}

// Config is the on-disk configuration.
type Config struct {
	Model   Model  `yaml:"model"`
	System  string `yaml:"system_prompt"`
	Budget  Budget `yaml:"budget"`
	Logging struct {
		Decisions bool `yaml:"decisions"`
	} `yaml:"logging"`
}

// Default returns the built-in defaults used when the file is absent
// or a field is missing.
func Default() Config {
	return Config{
		Model: Model{
			Name:      "deepseek-chat",
			BaseURL:   "https://api.deepseek.com/v1",
			APIKeyEnv: "OWLTIDE_API_KEY",
		},
		System: "You are OwlTide, a helpful personal agent. " +
			"Complete tasks using the provided tools. " +
			"Be concise. When the task is done, reply with a short summary. " +
			"When relaying sub-agent results, lead with the conclusion and keep " +
			"the default reply within about 10 lines; expand only on request. " +
			"Messages starting with [AUTO — not a new user message] are system-injected " +
			"restatements of your current goal, not new user instructions.",
		Budget: Budget{
			MaxTurns:       30,
			MaxTokens:      400000,
			ToolTimeoutSec: 60,
		},
		Logging: struct {
			Decisions bool `yaml:"decisions"`
		}{Decisions: true},
	}
}

// Path returns the config file location.
func Path() string {
	return filepath.Join(platform.ConfigDir(platform.New().Home()), "config.yaml")
}

// Load reads the config file, falling back to defaults for anything
// the file does not set. Missing file is not an error: defaults win.
func Load() (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(Path())
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", Path(), err)
	}
	if cfg.Model.BaseURL == "" || cfg.Model.Name == "" {
		return cfg, fmt.Errorf("config must set model.base_url and model.name")
	}
	return cfg, nil
}

// APIKey resolves the key from the configured environment variable.
func (c Config) APIKey() string {
	if c.Model.APIKeyEnv == "" {
		return os.Getenv("OWLTIDE_API_KEY")
	}
	return os.Getenv(c.Model.APIKeyEnv)
}

// WriteDefault writes an example config to p (used by `owltide init`).
func WriteDefault(p string) error {
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("%s already exists", p)
	}
	data, _ := yaml.Marshal(Default())
	return os.WriteFile(p, data, 0o600)
}
