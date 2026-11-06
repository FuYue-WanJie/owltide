package evolve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/FuYue-WanJie/owltide/core/store"
	"github.com/FuYue-WanJie/owltide/core/tool"
	"gopkg.in/yaml.v3"
)

func yamlUnmarshal(b []byte, v any) error { return yaml.Unmarshal(b, v) }

// knownTools returns the registered tool-name set for validation.
func (e *Env) knownTools() map[string]bool { return e.KnownToolsSet }

// ConfigPatch applies RFC 6902-style limited patches: set or remove
// of top-level yaml paths (dot notation, e.g. "budget.max_turns").
type ConfigPatch struct{ Env *Env }

// Schema implements tool.Tool.
func (ConfigPatch) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name: "config_patch",
		Description: "Patch ~/.owltide/config.yaml with dot-path set/remove ops, " +
			"e.g. {\"op\":\"set\",\"path\":\"budget.max_turns\",\"value\":10}. " +
			"Locked paths (config:budget, model identity) are rejected.",
		Parameters: json.RawMessage(`{"type":"object",
 "properties":{
   "op":{"type":"string","enum":["set","remove"]},
   "path":{"type":"string"},
   "value":{}
 },
 "required":["op","path"]}`),
	}}
}

// protectedConfigPaths are always rejected for the agent.
var protectedConfigPaths = map[string]bool{
	"budget":                  true,
	"budget.max_turns":        true,
	"budget.max_tokens":       true,
	"budget.tool_timeout_sec": true,
	"model":                   true,
	"model.name":              true,
	"model.base_url":          true,
	"model.api_key_env":       true,
}

// Call implements tool.Tool.
func (t ConfigPatch) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return reject("bad arguments: %v", err)
	}
	if a.Op != "set" && a.Op != "remove" {
		return reject("op must be set|remove")
	}
	if strings.TrimSpace(a.Path) == "" || strings.Contains(a.Path, "..") {
		return reject("invalid path")
	}
	if protectedConfigPaths[a.Path] || t.Env.St.IsLocked("config:"+a.Path) {
		return reject("config path %q is locked; only the user can change it", a.Path)
	}
	cfgPath := filepath.Join(t.Env.Root, "config.yaml")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		return reject("config.yaml missing; run owltide init first")
	}
	meta := store.CommitMeta{Who: "agent", Tool: "config.patch",
		Game: t.Env.SessionID, Reason: "config " + a.Op + " " + a.Path}
	return t.Env.commitWithRollback(ctx, meta, func() error {
		return patchYAMLFile(cfgPath, a.Op, strings.Split(a.Path, "."), a.Value)
	})
}

// patchYAMLFile edits a yaml file generically via node trees.
func patchYAMLFile(path, op string, parts []string, value json.RawMessage) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("empty config")
	}
	cur := root.Content[0]
	for i, p := range parts {
		last := i == len(parts)-1
		if cur.Kind != yaml.MappingNode {
			return fmt.Errorf("path %q traverses a non-mapping node", strings.Join(parts, "."))
		}
		idx := -1
		for j := 0; j+1 < len(cur.Content); j += 2 {
			if cur.Content[j].Value == p {
				idx = j + 1
				break
			}
		}
		if op == "remove" {
			if last {
				if idx < 0 {
					return fmt.Errorf("path %q not found", strings.Join(parts, "."))
				}
				cur.Content = append(cur.Content[:idx-1], cur.Content[idx+1:]...)
				return writeNode(path, &root)
			}
			if idx < 0 {
				return fmt.Errorf("path %q not found", strings.Join(parts, "."))
			}
			cur = cur.Content[idx]
			continue
		}
		// set
		if last {
			var v any
			if err := yaml.Unmarshal(value, &v); err != nil {
				return fmt.Errorf("value: %w", err)
			}
			nv := &yaml.Node{}
			if err := nv.Encode(v); err != nil {
				return err
			}
			if idx >= 0 {
				cur.Content[idx] = nv
			} else {
				k := &yaml.Node{}
				k.SetString(p)
				cur.Content = append(cur.Content, k, nv)
			}
			return writeNode(path, &root)
		}
		if idx < 0 {
			m := &yaml.Node{Kind: yaml.MappingNode}
			k := &yaml.Node{}
			k.SetString(p)
			cur.Content = append(cur.Content, k, m)
			cur = m
			continue
		}
		cur = cur.Content[idx]
	}
	return nil
}

func writeNode(path string, n *yaml.Node) error {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return writeFile(path, buf.String())
}

func writeFile(path, data string) error {
	return os.WriteFile(path, []byte(data), 0o600)
}
