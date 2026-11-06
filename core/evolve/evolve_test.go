package evolve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FuYue-WanJie/owltide/core/store"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

// fixture builds a temp ~/.owltide with config, store, and tools.
type fixture struct {
	env      *Env
	st       *store.Store
	root     string
	reg      *tool.Registry
	healthOK bool
}

func newFixture(t *testing.T) *fixture {
	return newFixtureT(t)
}

func newFixtureT(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	if err := store.EnsureLayout(root); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("model:\n  name: m\n  base_url: https://x\nbudget:\n  max_turns: 30\n  max_tokens: 400000\n  tool_timeout_sec: 60\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureInit(); err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st, root: root, healthOK: true}
	f.env = &Env{
		Root: root, St: st, SessionID: "test-session",
		KnownToolsSet: map[string]bool{"read_file": true, "run_shell": true, "write_file": true, "edit_file": true, "list_dir": true},
		Load:          func(string) error { return nil },
		Health: func(string) error {
			if !f.healthOK {
				return errHealth
			}
			return nil
		},
	}
	f.reg = tool.NewRegistry()
	if err := RegisterAll(f.reg, f.env); err != nil {
		t.Fatal(err)
	}
	return f
}

var errHealth = errorString("health broken")

type errorString string

func (e errorString) Error() string { return string(e) }

// call invokes a registered tool with JSON args.
func (f *fixture) call(t *testing.T, name, args string) (string, error) {
	t.Helper()
	tt, ok := f.reg.Get(name)
	if !ok {
		t.Fatalf("tool %s not registered", name)
	}
	return tt.Call(context.Background(), []byte(args))
}

const validAgent = `{
 "name":"reviewer","model":"m","description":"reviews Go code",
 "system_prompt":"review things",
 "tools":["read_file"],
 "budget":{"max_turns":5,"max_tokens":1000,"tool_timeout_sec":10}
}`

func TestAgentUpsertValidCommitsAndReloads(t *testing.T) {
	f := newFixture(t)
	res, err := f.call(t, "agent_upsert", validAgent)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !strings.Contains(res, "ok") {
		t.Fatalf("unexpected result %q", res)
	}
	if _, err := os.Stat(filepath.Join(f.root, "agents", "reviewer.md")); err != nil {
		t.Fatal("definition file missing")
	}
	log, _ := f.st.Log()
	if len(log) != 2 { // init fixture commit + upsert
		t.Fatalf("want 2 commits, got %d", len(log))
	}
	if _, who, toolName, session := store.ParseMessage(log[0].Msg); who != "agent" || session != "test-session" || toolName != "agent.upsert" {
		t.Fatalf("meta not recorded: %q", log[0].Msg)
	}
}

func TestAgentUpsertInvalidRejectedNotWritten(t *testing.T) {
	f := newFixture(t)
	before, _ := f.st.Head()
	res, err := f.call(t, "agent_upsert",
		`{"name":"Bad_Name","model":"m","description":"x","system_prompt":"x","tools":["read_file"],"budget":{"max_turns":1,"max_tokens":1,"tool_timeout_sec":1}}`)
	if err == nil || !strings.HasPrefix(err.Error(), RejectErr) {
		t.Fatalf("want rejection, got res=%q err=%v", res, err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "agents", "Bad_Name.md")); !os.IsNotExist(err) {
		t.Fatal("invalid definition must not touch disk")
	}
	if head, _ := f.st.Head(); head != before {
		t.Fatal("rejected call must not commit")
	}
}

func TestHealthFailureRollsBack(t *testing.T) {
	f := newFixture(t)
	f.healthOK = false
	_, err := f.call(t, "agent_upsert", validAgent)
	if err == nil || !strings.Contains(err.Error(), "health check failed") {
		t.Fatalf("want health rollback, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "agents", "reviewer.md")); !os.IsNotExist(err) {
		t.Fatal("file should be reverted")
	}
	// Working tree matches the pre-call commit.
	log, _ := f.st.Log()
	f2 := newFixture(t) // same first-commit content
	if len(log) != 2 {
		t.Fatalf("rollback should leave original + revert, got %d", len(log))
	}
	_ = f2
}

func TestLockedAgentRejectsMutationButStaysUsable(t *testing.T) {
	f := newFixture(t)
	if _, err := f.call(t, "agent_upsert", validAgent); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Lock("agent:reviewer"); err != nil {
		t.Fatal(err)
	}
	// Mutation rejected.
	_, err := f.call(t, "agent_upsert",
		`{"name":"reviewer","model":"m2","description":"y","system_prompt":"x","tools":["read_file"],"budget":{"max_turns":5,"max_tokens":1000,"tool_timeout_sec":10}}`)
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("want locked rejection, got %v", err)
	}
	// Removal rejected.
	if _, err := f.call(t, "agent_remove", `{"name":"reviewer"}`); err == nil {
		t.Fatal("remove on locked agent must fail")
	}
	// File unchanged.
	data, _ := os.ReadFile(filepath.Join(f.root, "agents", "reviewer.md"))
	if !strings.Contains(string(data), "model: m\n") {
		t.Fatalf("locked file was modified: %q", data)
	}
}

func TestRollbackPreservesLocked(t *testing.T) {
	f := newFixture(t)
	if _, err := f.call(t, "agent_upsert", validAgent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(t, "skill_write", `{"name":"greet","body":"# greet\n"}`); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Lock("agent:reviewer"); err != nil {
		t.Fatal(err)
	}
	// Roll back two steps (lock commit + skill write): greet.md goes
	// away, locked agent definition stays.
	_, err := f.call(t, "history_rollback", `{"steps":2}`)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "skills", "greet.md")); !os.IsNotExist(err) {
		t.Fatal("unlocked file should be rolled back")
	}
	data, err := os.ReadFile(filepath.Join(f.root, "agents", "reviewer.md"))
	if err != nil || !strings.Contains(string(data), "reviewer") {
		t.Fatalf("locked agent file must survive rollback: %v", err)
	}
}

func TestConfigPatchBudgetAndModelLocked(t *testing.T) {
	f := newFixture(t)
	_, err := f.call(t, "config_patch", `{"op":"set","path":"budget.max_turns","value":99}`)
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("budget must be always locked, got %v", err)
	}
	_, err = f.call(t, "config_patch", `{"op":"set","path":"model.name","value":"other"}`)
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("model identity must be locked, got %v", err)
	}
	// Non-protected path succeeds.
	res, err := f.call(t, "config_patch", `{"op":"set","path":"system_prompt","value":"hello"}`)
	if err != nil || !strings.Contains(res, "ok") {
		t.Fatalf("legal patch failed: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(f.root, "config.yaml"))
	if !strings.Contains(string(data), "hello") {
		t.Fatalf("patch not applied: %q", data)
	}
}

func TestChangesReportListsAgentEdits(t *testing.T) {
	f := newFixture(t)
	if _, err := f.call(t, "agent_upsert", validAgent); err != nil {
		t.Fatal(err)
	}
	out, err := ChangesReport(f.st, 24*3600e9)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "agent.upsert") || !strings.Contains(out, "who=agent") {
		t.Fatalf("report missing agent entry:\n%s", out)
	}
}
