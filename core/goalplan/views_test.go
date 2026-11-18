package goalplan

// P2b-M3 tests: shared read views (CLI and serve consume the same
// functions) and the store report (counts + frontmatter failures,
// report-only).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoalViewsWithPlans(t *testing.T) {
	m := newMgr(t)
	g := mustCreateGoal(t, m, "ship", "Ship the feature\n- [ ] tests")
	// The plan must reference THIS goal (mustCreatePlan creates its own).
	plan, err := m.Create(KindPlan, "ship-steps", "s-test", "- [ ] implement", g.ID)
	if err != nil {
		t.Fatal(err)
	}
	views, err := m.GoalViews()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views = %d", len(views))
	}
	v := views[0]
	if v.ID != g.ID || v.Status != StatusActive || v.Title != "Ship the feature" {
		t.Fatalf("view = %+v", v)
	}
	if len(v.Plans) != 1 || v.Plans[0] != plan.ID {
		t.Fatalf("plans = %v", v.Plans)
	}
	if v.Updated == "" {
		t.Fatal("updated missing")
	}
	plans, err := m.PlanViews()
	if err != nil || len(plans) != 1 || plans[0].Goal != g.ID {
		t.Fatalf("plan views = %v err=%v", plans, err)
	}
}

func TestTitleSkipsCheckboxes(t *testing.T) {
	if got := Title("- [ ] step\n- [x] done\n\nReal title\nmore"); got != "Real title" {
		t.Fatalf("title = %q", got)
	}
	if got := Title(""); got != "(untitled)" {
		t.Fatalf("empty title = %q", got)
	}
	long := strings.Repeat("x", 120)
	if got := Title(long); len([]rune(got)) != 81 { // 80 + ellipsis
		t.Fatalf("truncated title runes = %d", len([]rune(got)))
	}
}

func TestReportCountsAndInvalid(t *testing.T) {
	m := newMgr(t)
	mustCreateGoal(t, m, "ok", "fine")
	mustCreatePlan(t, m, "ok-plan", "- [x] done")

	// A broken document: missing frontmatter.
	badDir := filepath.Join(m.Dir, "goals")
	if err := os.WriteFile(filepath.Join(badDir, "20260101-broken.md"),
		[]byte("no frontmatter here"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A truncated frontmatter document.
	if err := os.WriteFile(filepath.Join(m.Dir, "plans", "20260102-torn.md"),
		[]byte("---\nkind: plan\nid: p-0000"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A session todo.md without frontmatter.
	todoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(todoDir, "todo.md"), []byte("- [ ] legacy"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := m.Report(todoDir)
	if err != nil {
		t.Fatal(err)
	}
	// goals: ok + broken; plans: ok-plan + torn.
	// goals: ok + mustCreatePlan's own goal + broken = 3.
	if rep.Goals != 3 || rep.Plans != 2 || rep.Todos != 1 {
		t.Fatalf("counts = %+v", rep)
	}
	if len(rep.Invalid) != 3 {
		t.Fatalf("invalid = %d (%v)", len(rep.Invalid), rep.Invalid)
	}
	// Validation is report-only: the broken files stay untouched.
	data, _ := os.ReadFile(filepath.Join(badDir, "20260101-broken.md"))
	if string(data) != "no frontmatter here" {
		t.Fatal("report must never repair files")
	}
}

func TestReportCleanStore(t *testing.T) {
	m := newMgr(t)
	mustCreateGoal(t, m, "clean", "body")
	rep, err := m.Report("")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Goals != 1 || rep.Plans != 0 || len(rep.Invalid) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}
