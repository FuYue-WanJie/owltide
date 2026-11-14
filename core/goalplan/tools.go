package goalplan

// Agent tools for the goal/plan documents (P2b-M1): goal_create,
// goal_update, plan_create, plan_update. Router-mode coordination is a
// legitimate use: the main agent's AllowedTools carries these names,
// and refusals there would be wrong (they are not router violations —
// documented in p2b.md §1.2).
//
// Frontmatter protection: the tools write whole documents through
// Manager (render always emits a valid frontmatter), while the guard's
// protected set blocks run_shell redirection and write_file/edit_file
// from touching goals/** and plans/** frontmatter (see tools_test /
// guard wiring in cmd/owltide).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FuYue-WanJie/owltide/core/decision"
	"github.com/FuYue-WanJie/owltide/core/tool"
	"github.com/FuYue-WanJie/owltide/internal/perm"
)

// Hooks carries the collaborators the tools need.
type Hooks struct {
	M *Manager
	// SessionID is the creating session.
	SessionID string
	// Perm is the permission service (blocked -> active confirmation).
	Perm *perm.Service
	// Events writes decisions events (goal_update/plan_update); nil
	// disables.
	Events func(event string, fields map[string]any)
	// OnStoreChange is called after a successful write (auto-commit).
	OnStoreChange func(reason string)
	// ConfirmUnblock performs the user confirmation for blocked ->
	// active. It must route through perm (kind goal_unblock) and may
	// be nil in tests that pass forceApprove.
	ConfirmUnblock func(goalID, reason string) (approved bool, err error)
}

func (h *Hooks) event(name string, fields map[string]any) {
	if h.Events != nil {
		h.Events(name, fields)
	}
}

func (h *Hooks) commit(reason string) {
	if h.OnStoreChange != nil {
		h.OnStoreChange(reason)
	}
}

// ---- goal_create ----

type goalCreate struct{ h *Hooks }

func (goalCreate) Caps() tool.Caps { return tool.CapWrites }

func (goalCreate) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name: "goal_create",
		Description: "Create a goal document in the config store (goals/<date>-<slug>.md). " +
			"The body carries the goal statement plus checkbox acceptance criteria (- [ ]). " +
			"Cross-file references use ids only (goal:<id> / plan:<id>).",
		Parameters: json.RawMessage(`{"type":"object",` +
			`"properties":{"slug":{"type":"string","description":"file name slug, [a-z0-9-]"},` +
			`"body":{"type":"string","description":"goal statement + acceptance criteria"}},` +
			`"required":["slug","body"]}`),
	}}
}

func (t goalCreate) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Slug string `json:"slug"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	d, err := t.h.M.Create(KindGoal, a.Slug, t.h.SessionID, a.Body, "")
	if err != nil {
		return "", err
	}
	t.h.commit("goal_create " + d.ID)
	return fmt.Sprintf("created %s (%s), status %s", d.ID, d.FileName, d.Status), nil
}

// ---- plan_create ----

type planCreate struct{ h *Hooks }

func (planCreate) Caps() tool.Caps { return tool.CapWrites }

func (planCreate) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name: "plan_create",
		Description: "Create a plan for a goal (plans/<date>-<slug>.md). The body is an " +
			"ordered step list; use todo checkboxes (- [ ] / - [x]) for steps that must " +
			"all be checked before the plan can be set done. Reference the goal by id.",
		Parameters: json.RawMessage(`{"type":"object",` +
			`"properties":{"slug":{"type":"string"},` +
			`"goal":{"type":"string","description":"goal id (g-...)"},` +
			`"body":{"type":"string"}},` +
			`"required":["slug","goal","body"]}`),
	}}
}

func (t planCreate) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Slug string `json:"slug"`
		Goal string `json:"goal"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	// The goal must exist: ids are the only cross-reference currency.
	if _, err := t.h.M.Load(a.Goal); err != nil {
		return "", fmt.Errorf("goal %q not found: %w", a.Goal, err)
	}
	d, err := t.h.M.Create(KindPlan, a.Slug, t.h.SessionID, a.Body, a.Goal)
	if err != nil {
		return "", err
	}
	t.h.commit("plan_create " + d.ID)
	return fmt.Sprintf("created %s (%s) for %s, status %s", d.ID, d.FileName, a.Goal, d.Status), nil
}

// ---- goal_update / plan_update ----

type updateTool struct {
	h    *Hooks
	kind string
}

func (updateTool) Caps() tool.Caps { return tool.CapWrites }

func (t updateTool) Schema() tool.Definition {
	name := "goal_update"
	extra := ""
	if t.kind == KindPlan {
		name = "plan_update"
		extra = `,"todos_done":{"type":"boolean","description":"confirm every - [ ] step is checked when setting status=done"}`
	}
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name: name,
		Description: "Update a " + t.kind + " document's body or status. Transitions: " +
			"active -> done | blocked (blocked needs a first body line 'blocked: <reason>') " +
			"-> archived; blocked -> active requires user confirmation " +
			"(timeout or refusal keeps it blocked). Illegal transitions are refused " +
			"and the file is left untouched.",
		Parameters: json.RawMessage(`{"type":"object",` +
			`"properties":{"id":{"type":"string"},` +
			`"status":{"type":"string","enum":["active","done","blocked","archived"]},` +
			`"body":{"type":"string","description":"new body (frontmatter is protected)"}` + extra +
			`},"required":["id"]}`),
	}}
}

func (t updateTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		Body      string `json:"body"`
		TodosDone *bool  `json:"todos_done"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}

	// Body rewrite (no status change): bodies are editable, the
	// frontmatter never leaves the render path.
	if a.Body != "" {
		d, err := t.h.M.Load(a.ID)
		if err != nil {
			return "", err
		}
		if d.Kind != t.kind {
			return "", fmt.Errorf("%s is a %s, not a %s", a.ID, d.Kind, t.kind)
		}
		d.Body = strings.TrimSpace(a.Body)
		if d.Status == StatusBlocked && !strings.HasPrefix(d.Body, blockedPrefix) {
			return "", ErrBlockedNeedsReason
		}
		if err := t.h.M.Save(d); err != nil {
			return "", err
		}
		t.h.commit(t.kind + "_update " + d.ID + " (body)")
		if a.Status == "" || a.Status == d.Status {
			return fmt.Sprintf("%s body updated (%s)", d.ID, d.Status), nil
		}
	}

	if a.Status == "" {
		return "", fmt.Errorf("nothing to update: give body and/or status")
	}

	// Status transition.
	if a.Status == StatusActive {
		d, err := t.h.M.Load(a.ID)
		if err != nil {
			return "", err
		}
		if d.Status == StatusBlocked {
			return t.unblock(a.ID)
		}
	}

	d, from, needConfirm, err := t.h.M.SetStatus(a.ID, a.Status, func(planID string) (bool, error) {
		if a.TodosDone != nil && *a.TodosDone {
			// The model asserts all todos are done; the document must
			// agree (no "- [ ]" left).
			doc, err := t.h.M.Load(planID)
			if err != nil {
				return false, err
			}
			return TodosAllDone(doc.Body), nil
		}
		doc, err := t.h.M.Load(planID)
		if err != nil {
			return false, err
		}
		return TodosAllDone(doc.Body), nil
	})
	if err != nil {
		return "", err
	}
	if needConfirm {
		return t.unblock(a.ID)
	}
	t.h.commit(t.kind + "_update " + d.ID + " -> " + d.Status)
	t.h.event(t.kind+"_update", map[string]any{
		"id": d.ID, "from": from, "to": d.Status,
	})
	return fmt.Sprintf("%s status: %s", d.ID, d.Status), nil
}

// unblock runs the user-confirmed blocked -> active move through the
// permission service (kind goal_unblock): timeout or denial keeps the
// goal blocked; a social channel is refused outright (read-only).
func (t updateTool) unblock(id string) (string, error) {
	d, err := t.h.M.Load(id)
	if err != nil {
		return "", err
	}
	reason := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(d.Body, "\n", 2)[0], blockedPrefix))
	if t.h.ConfirmUnblock == nil {
		return "", fmt.Errorf("goalplan: no confirmation path wired; %s stays blocked", id)
	}
	approved, err := t.h.ConfirmUnblock(id, reason)
	if err != nil {
		// Timeout/refusal keep blocked; the error text carries the state.
		return "", fmt.Errorf("%s stays blocked (unblock not confirmed: %v)", id, err)
	}
	if !approved {
		return "", fmt.Errorf("%s stays blocked (user declined)", id)
	}
	d, err = t.h.M.ApplyUnblock(id)
	if err != nil {
		return "", err
	}
	t.h.commit("goal_unblock " + d.ID)
	t.h.event("goal_update", map[string]any{
		"id": d.ID, "from": StatusBlocked, "to": StatusActive, "reason": "user_confirmed",
	})
	return fmt.Sprintf("%s unblocked (user confirmed)", d.ID), nil
}

// NewTools registers the four tools on a registry and returns the
// hooks (wired by the caller with session/perm/event collaborators).
func NewTools(reg *tool.Registry, h *Hooks) error {
	for _, tl := range []tool.Tool{
		goalCreate{h}, planCreate{h},
		updateTool{h, KindGoal}, updateTool{h, KindPlan},
	} {
		if err := reg.Register(tl); err != nil {
			return err
		}
	}
	return nil
}

// EmitUpdateEvent is the shared decisions-event helper (M2): one line
// per transition, no body text.
func EmitUpdateEvent(fn func(string, map[string]any), kind, id, from, to, reason string) {
	if fn == nil {
		return
	}
	f := map[string]any{"id": id, "from": from, "to": to}
	if reason != "" {
		f["reason"] = reason
	}
	fn(kind+"_update", f)
}

// ToolsRegistered lists the tool names (used by setup to extend the
// router allowlist: coordination is not a violation).
func ToolsRegistered() []string {
	return []string{"goal_create", "goal_update", "plan_create", "plan_update"}
}

var _ = decision.AppendEvent // keep import aligned with the M2 event path
