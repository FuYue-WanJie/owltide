package main

// P2b-M3: `owltide goals` — list the current workspace's goals (id,
// status, title, linked plans, updated) with an optional --json form,
// plus a store health report (goal/plan/todo counts and frontmatter
// validation failures; report-only, never repairs).

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/FuYue-WanJie/owltide/core/goalplan"
	"github.com/FuYue-WanJie/owltide/core/platform"
)

// storeDir is the config-store root (~/.owltide), matching setup().
func storeDir() string { return platform.ConfigDir(platform.New().Home()) }

// runGoalsCmd implements the subcommand.
func runGoalsCmd(args []string) {
	asJSON := false
	dir := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--json":
			asJSON = true
		case args[i] == "--dir" && i+1 < len(args):
			dir = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--dir="):
			dir = strings.TrimPrefix(args[i], "--dir=")
		case args[i] == "--help" || args[i] == "-h":
			fmt.Println("usage: owltide goals [--json] [--dir <config-dir>]")
			return
		default:
			fmt.Fprintf(os.Stderr, "owltide goals: unknown argument %q\n", args[i])
			fmt.Fprintln(os.Stderr, "usage: owltide goals [--json] [--dir <config-dir>]")
			os.Exit(2)
		}
	}
	if dir == "" {
		dir = storeDir()
	}
	m := goalplan.New(dir)
	goals, err := m.GoalViews()
	if err != nil {
		must(err)
	}
	rep, err := m.Report("") // todo.md is session-scoped; the store report covers goals/plans
	if err != nil {
		must(err)
	}

	if asJSON {
		out := struct {
			Goals  []goalplan.GoalView `json:"goals"`
			Report goalplan.Report     `json:"report"`
		}{Goals: goals, Report: rep}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return
	}

	if len(goals) == 0 {
		fmt.Println("（没有 goal 文档）")
	} else {
		fmt.Printf("%-12s %-9s %-28s %-14s %s\n", "ID", "STATUS", "PLANS", "UPDATED", "TITLE")
		for _, g := range goals {
			plans := strings.Join(g.Plans, ",")
			if plans == "" {
				plans = "-"
			}
			fmt.Printf("%-12s %-9s %-28s %-14s %s\n", g.ID, g.Status, plans, g.Updated, g.Title)
		}
	}
	fmt.Printf("\nstore: goals=%d plans=%d todos=%d\n", rep.Goals, rep.Plans, rep.Todos)
	if len(rep.Invalid) == 0 {
		fmt.Println("frontmatter: all valid")
	} else {
		fmt.Printf("frontmatter failures (%d):\n", len(rep.Invalid))
		for _, e := range rep.Invalid {
			fmt.Printf("  %s: %s\n", e.File, e.Reason)
		}
	}
}
