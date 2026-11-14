package agent

// P2b-M2: goal/plan injection rides the per-call view (after the
// stable prefix); the request prefix hash stays byte-identical.

import (
	"testing"

	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

// TestGoalInjectAfterPrefix: with GoalInject set, the injection appears
// in the model VIEW but the prefix hash is unchanged.
func TestGoalInjectAfterPrefix(t *testing.T) {
	reg := tool.NewRegistry()
	cfg := config.Default()
	cfg.Model.BaseURL = "https://x"
	opts := agentOptions(cfg, model.NewClient("https://x", "k", nil), reg, nil)

	a := New(opts)
	before := a.prefixHash
	viewBefore := len(a.viewForModel())

	// Set the injector and make sure it produces output.
	calls := 0
	a.opts.GoalInject = func() string {
		calls++
		return "[AUTO — not a new user message] 当前 goal / plan\nGOAL g-0123abcd [active]"
	}
	after := a.prefixHash
	if after != before {
		t.Fatal("GoalInject must not change the prefix hash")
	}
	view := a.viewForModel()
	if len(view) != viewBefore+1 {
		t.Fatalf("view grew by %d, want 1", len(view)-viewBefore)
	}
	last := view[len(view)-1]
	if last.Role != model.RoleUser || last.Content == "" {
		t.Fatalf("injected message wrong: %+v", last)
	}
	if calls == 0 {
		t.Fatal("injector was not called")
	}
}

// TestGoalInjectEmptyOmitsMessage: an empty injection adds nothing.
func TestGoalInjectEmptyOmitsMessage(t *testing.T) {
	reg := tool.NewRegistry()
	cfg := config.Default()
	cfg.Model.BaseURL = "https://x"
	opts := agentOptions(cfg, model.NewClient("https://x", "k", nil), reg, nil)
	a := New(opts)
	before := len(a.viewForModel())
	a.opts.GoalInject = func() string { return "" }
	if got := len(a.viewForModel()); got != before {
		t.Fatalf("view grew by %d with an empty injection", got-before)
	}
}
