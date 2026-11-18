package goalplan

// Shared read views (P2b-M3): the CLI (`owltide goals`) and the serve
// API (GET /api/goals, /api/plans) consume these same functions so the
// two surfaces can never disagree.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GoalView is one goal row for the CLI/API.
type GoalView struct {
	ID      string   `json:"id"`
	Status  string   `json:"status"`
	Title   string   `json:"title"`
	File    string   `json:"file"`
	Session string   `json:"session"`
	Updated string   `json:"updated"`
	Plans   []string `json:"plans"` // plan ids referencing this goal
}

// PlanView is one plan row.
type PlanView struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Title   string `json:"title"`
	File    string `json:"file"`
	Goal    string `json:"goal"`
	Session string `json:"session"`
	Updated string `json:"updated"`
}

// GoalViews lists goals with their plan ids (same read path as serve).
func (m *Manager) GoalViews() ([]GoalView, error) {
	docs, err := m.List("")
	if err != nil {
		return nil, err
	}
	byGoal := map[string][]string{}
	for _, d := range docs {
		if d.Kind == KindPlan && d.Goal != "" {
			byGoal[d.Goal] = append(byGoal[d.Goal], d.ID)
		}
	}
	out := make([]GoalView, 0)
	for _, d := range docs {
		if d.Kind != KindGoal {
			continue
		}
		plans := byGoal[d.ID]
		sort.Strings(plans)
		if plans == nil {
			plans = []string{}
		}
		out = append(out, GoalView{
			ID: d.ID, Status: d.Status, Title: Title(d.Body), File: d.FileName,
			Session: d.Session, Updated: d.Updated.UTC().Format("2006-01-02T15:04:05Z"),
			Plans: plans,
		})
	}
	return out, nil
}

// PlanViews lists plans.
func (m *Manager) PlanViews() ([]PlanView, error) {
	docs, err := m.List(KindPlan)
	if err != nil {
		return nil, err
	}
	out := make([]PlanView, 0, len(docs))
	for _, d := range docs {
		out = append(out, PlanView{
			ID: d.ID, Status: d.Status, Title: Title(d.Body), File: d.FileName,
			Goal: d.Goal, Session: d.Session,
			Updated: d.Updated.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return out, nil
}

// Title is the first meaningful line of a document body (checkbox
// lines and blanks skipped), truncated for display.
func Title(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "- [") {
			continue
		}
		if r := []rune(t); len(r) > 80 {
			return string(r[:80]) + "…"
		}
		return t
	}
	return "(untitled)"
}

// Report is the store health report (P2b-M3): counts per kind plus
// frontmatter validation failures. Reporting only — never repairs.
type Report struct {
	Goals   int            `json:"goals"`
	Plans   int            `json:"plans"`
	Todos   int            `json:"todos"`
	Invalid []InvalidEntry `json:"invalid"`
}

// InvalidEntry is one unparseable document.
type InvalidEntry struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// Report scans the store (goals/, plans/) plus a session todo.md when
// its directory is given ("" skips todos).
func (m *Manager) Report(todoDir string) (Report, error) {
	var rep Report
	for _, kind := range []string{KindGoal, KindPlan} {
		dir := filepath.Join(m.Dir, m.subdir(kind))
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return rep, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if kind == KindGoal {
				rep.Goals++
			} else {
				rep.Plans++
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				rep.Invalid = append(rep.Invalid, InvalidEntry{File: e.Name(), Reason: err.Error()})
				continue
			}
			if _, err := parse(filepath.Join(dir, e.Name()), e.Name(), string(data)); err != nil {
				rep.Invalid = append(rep.Invalid, InvalidEntry{File: kind + "/" + e.Name(), Reason: err.Error()})
			}
		}
	}
	if todoDir != "" {
		path := filepath.Join(todoDir, "todo.md")
		if data, err := os.ReadFile(path); err == nil {
			rep.Todos++
			text := string(data)
			if !strings.HasPrefix(strings.TrimSpace(text), "---") {
				rep.Invalid = append(rep.Invalid, InvalidEntry{
					File: "todo.md", Reason: "missing frontmatter (legacy file, upgraded on the first write)",
				})
			}
		}
	}
	sort.Slice(rep.Invalid, func(i, j int) bool { return rep.Invalid[i].File < rep.Invalid[j].File })
	if rep.Invalid == nil {
		rep.Invalid = []InvalidEntry{}
	}
	return rep, nil
}
