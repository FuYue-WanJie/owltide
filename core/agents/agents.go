// Package agents defines the sub-agent configuration schema,
// validation, and loading from ~/.owltide/agents/*.yaml.
package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Def is one sub-agent definition (agents/<name>.yaml).
type Def struct {
	Name         string   `yaml:"name"`
	Model        string   `yaml:"model"`
	BaseURL      string   `yaml:"base_url"` // optional, defaults to global
	SystemPrompt string   `yaml:"system_prompt"`
	Tools        []string `yaml:"tools"`
	Budget       Budget   `yaml:"budget"`
}

// Budget caps one sub-agent run.
type Budget struct {
	MaxTurns       int `yaml:"max_turns"`
	MaxTokens      int `yaml:"max_tokens"`
	ToolTimeoutSec int `yaml:"tool_timeout_sec"`
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Validate checks one definition against the schema.
func (d Def) Validate(knownTools map[string]bool) error {
	if !nameRe.MatchString(d.Name) {
		return fmt.Errorf("name %q must match %s", d.Name, nameRe)
	}
	if d.Model == "" {
		return fmt.Errorf("model is required")
	}
	if d.SystemPrompt == "" {
		return fmt.Errorf("system_prompt is required")
	}
	if len(d.Tools) == 0 {
		return fmt.Errorf("tools must be a non-empty list")
	}
	for _, tn := range d.Tools {
		if knownTools != nil && !knownTools[tn] {
			return fmt.Errorf("unknown tool %q", tn)
		}
	}
	if d.Budget.MaxTurns <= 0 || d.Budget.MaxTokens <= 0 || d.Budget.ToolTimeoutSec <= 0 {
		return fmt.Errorf("budget fields must all be positive")
	}
	return nil
}

// LockID is the protected-set id for a sub-agent.
func (d Def) LockID() string { return "agent:" + d.Name }

// Dir returns agents dir under the config root.
func Dir(root string) string { return filepath.Join(root, "agents") }

// Load reads and validates every agents/*.yaml under root.
// knownTools is the set of registered tool names; nil skips that check.
func Load(root string, knownTools map[string]bool) (map[string]Def, error) {
	out := map[string]Def{}
	entries, err := os.ReadDir(Dir(root))
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(Dir(root), e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var d Def
		if err := yaml.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if err := d.Validate(knownTools); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if _, dup := out[d.Name]; dup {
			return nil, fmt.Errorf("duplicate agent name %q", d.Name)
		}
		out[d.Name] = d
	}
	return out, nil
}

// Save writes one definition to agents/<name>.yaml.
func Save(root string, d Def) error {
	if err := d.Validate(nil); err != nil {
		return err
	}
	data, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(Dir(root), d.Name+".yaml"), data, 0o600)
}

// Remove deletes agents/<name>.yaml; missing fails.
func Remove(root, name string) error {
	p := filepath.Join(Dir(root), name+".yaml")
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return fmt.Errorf("agent %q does not exist", name)
	}
	return os.Remove(p)
}

// Names lists agent names, sorted (for health checks and tool results).
func Names(defs map[string]Def) []string {
	var out []string
	for n := range defs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
