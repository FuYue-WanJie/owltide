package goalplan

// Session goal/plan injection (P2b-M2): active goals (id, statement,
// unchecked acceptance criteria) and active plans (current steps) ride
// the [AUTO — not a new user message] channel, computed fresh per model
// call and never part of the stable prefix. Total length cap: 2000
// characters — beyond it only ids and first lines are listed.

import (
	"fmt"
	"sort"
	"strings"
)

// InjectBudget caps the injection text.
const InjectBudget = 2000

// Injector produces the per-call injection for one store.
type Injector struct{ M *Manager }

// Inject renders the active goal/plan summary ("" when nothing is
// active).
func (in Injector) Inject() string {
	if in.M == nil {
		return ""
	}
	docs, err := in.M.List("")
	if err != nil {
		return ""
	}
	var goals, plans []Doc
	for _, d := range docs {
		switch d.Status {
		case StatusActive, StatusBlocked:
			if d.Kind == KindGoal {
				goals = append(goals, d)
			} else {
				plans = append(plans, d)
			}
		}
	}
	if len(goals) == 0 && len(plans) == 0 {
		return ""
	}
	sort.Slice(goals, func(i, j int) bool { return goals[i].FileName < goals[j].FileName })
	sort.Slice(plans, func(i, j int) bool { return plans[i].FileName < plans[j].FileName })

	full := renderInjection(goals, plans, false)
	if len(full) <= InjectBudget {
		return full
	}
	brief := renderInjection(goals, plans, true)
	if len(brief) > InjectBudget {
		// Even the brief form overflows: ids only.
		var b strings.Builder
		b.WriteString("[AUTO — not a new user message] 当前 goal/plan（仅 id）\n")
		for _, d := range append(append([]Doc{}, goals...), plans...) {
			fmt.Fprintf(&b, "%s %s\n", d.ID, d.FileName)
		}
		return strings.TrimRight(b.String(), "\n")
	}
	return brief
}

func renderInjection(goals, plans []Doc, brief bool) string {
	var b strings.Builder
	b.WriteString("[AUTO — not a new user message] 当前 goal / plan（系统注入，非新用户消息）\n")
	for _, g := range goals {
		fmt.Fprintf(&b, "\nGOAL %s [%s]\n", g.ID, g.Status)
		if brief {
			line := firstMeaningfulLine(g.Body)
			if line != "" {
				fmt.Fprintf(&b, "  %s\n", truncate(line, 120))
			}
			continue
		}
		b.WriteString(strings.TrimRight(g.Body, "\n") + "\n")
	}
	for _, p := range plans {
		fmt.Fprintf(&b, "\nPLAN %s [%s]", p.ID, p.Status)
		if p.Goal != "" {
			fmt.Fprintf(&b, " (goal %s)", p.Goal)
		}
		b.WriteString("\n")
		if brief {
			line := firstMeaningfulLine(p.Body)
			if line != "" {
				fmt.Fprintf(&b, "  %s\n", truncate(line, 120))
			}
			continue
		}
		b.WriteString(strings.TrimRight(p.Body, "\n") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// firstMeaningfulLine returns the first non-empty, non-checkbox line.
func firstMeaningfulLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "- [") {
			return t
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
