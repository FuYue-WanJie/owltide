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

	"github.com/FuYue-WanJie/owltide/core/tool"
)

// Def is one sub-agent definition (agents/<name>.md):
// YAML frontmatter for the machine fields, markdown body as the
// system prompt.
type Def struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Model        string   `yaml:"model"`
	BaseURL      string   `yaml:"base_url"` // optional, defaults to global
	SystemPrompt string   `yaml:"-"`        // markdown body
	Tools        []string `yaml:"tools"`
	Budget       Budget   `yaml:"budget"`
	// Builtin marks one of the four seeded roles: users may edit but
	// not delete; self-evolution may never widen their tool whitelist
	// or drop their readonly flag.
	Builtin bool `yaml:"builtin,omitempty"`
	// BuiltinVersion records which shipped builtin generation a seeded
	// file carries; SeedBuiltins uses content hashes to upgrade
	// untouched files, so this field is informational.
	BuiltinVersion int `yaml:"builtin_version,omitempty"`
	// Readonly roles receive only tools declared as read-only at
	// registration; any tool lacking an explicit read-only declaration
	// is treated as writing and refused system-side.
	Readonly bool `yaml:"readonly,omitempty"`
}

// frontmatter wraps fields as a markdown header block.
func (d Def) frontmatter() string {
	fm := d
	fm.SystemPrompt = ""
	data, _ := yaml.Marshal(fm)
	return string(data)
}

// render produces the full markdown document.
func (d Def) render() string {
	return "---\n" + d.frontmatter() + "---\n\n" + strings.TrimSpace(d.SystemPrompt) + "\n"
}

// parseDef decodes a .md document into a Def.
func parseDef(data []byte) (Def, error) {
	var d Def
	text := strings.TrimSpace(string(data))
	if !strings.HasPrefix(text, "---") {
		return d, fmt.Errorf("missing frontmatter")
	}
	rest := strings.TrimPrefix(text, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return d, fmt.Errorf("unclosed frontmatter")
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &d); err != nil {
		return d, err
	}
	d.SystemPrompt = strings.TrimSpace(rest[end+4:])
	return d, nil
}

// Budget caps one sub-agent run.
type Budget struct {
	MaxTurns       int `yaml:"max_turns" json:"max_turns"`
	MaxTokens      int `yaml:"max_tokens" json:"max_tokens"`
	ToolTimeoutSec int `yaml:"tool_timeout_sec" json:"tool_timeout_sec"`
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Validate checks one definition against the schema.
func (d Def) Validate(knownTools map[string]bool) error {
	if !nameRe.MatchString(d.Name) {
		return fmt.Errorf("name %q must match %s", d.Name, nameRe)
	}
	if d.Description == "" {
		return fmt.Errorf("description is required")
	}
	if d.Model == "" {
		return fmt.Errorf("model is required")
	}
	if d.SystemPrompt == "" {
		return fmt.Errorf("system prompt body is required")
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(Dir(root), e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		d, err := parseDef(data)
		if err != nil {
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

// Save writes one definition to agents/<name>.md.
func Save(root string, d Def) error {
	if err := d.Validate(nil); err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(root), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(Dir(root), d.Name+".md"), []byte(d.render()), 0o600)
}

// Remove deletes agents/<name>.md; missing fails.
func Remove(root, name string) error {
	p := filepath.Join(Dir(root), name+".md")
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return fmt.Errorf("agent %q does not exist", name)
	}
	return os.Remove(p)
}

// FilterRegistry builds a registry exposing only the tools this
// sub-agent whitelists; this is the enforcement point a scheduler
// uses when spawning the sub-agent.
func (d Def) FilterRegistry(reg *tool.Registry) (*tool.Registry, error) {
	out := tool.NewRegistry()
	for _, name := range d.Tools {
		t, ok := reg.Get(name)
		if !ok {
			return nil, fmt.Errorf("tool %q not registered", name)
		}
		if err := out.Register(t); err != nil {
			return nil, err
		}
	}
	return out, nil
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
