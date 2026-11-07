// Command owltide is the CLI entry: chat (interactive) and run (one-shot).
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/FuYue-WanJie/owltide/core/agent"
	"github.com/FuYue-WanJie/owltide/core/agents"
	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/decision"
	"github.com/FuYue-WanJie/owltide/core/evolve"
	"github.com/FuYue-WanJie/owltide/core/guard"
	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/platform"
	"github.com/FuYue-WanJie/owltide/core/sessionlog"
	"github.com/FuYue-WanJie/owltide/core/spill"
	"github.com/FuYue-WanJie/owltide/core/store"
	"github.com/FuYue-WanJie/owltide/core/subagent"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

const usage = `OwlTide - a personal agent.

Usage:
  owltide init            write an example config to ~/.owltide/config.yaml
  owltide chat            interactive multi-turn session
  owltide run "task"      run one task and exit
  owltide lock <id>       protect a config item (user only)
  owltide unlock <id>     release a protection (user only)
  owltide changes         list what the agent changed about itself
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		must(store.EnsureLayout(platform.ConfigDir(platform.New().Home())))
		must(config.WriteDefault(config.Path()))
		fmt.Println("wrote", config.Path())
	case "chat":
		runChat()
	case "run":
		if len(os.Args) < 3 {
			fmt.Println(`usage: owltide run "task"`)
			os.Exit(2)
		}
		runOneShot(strings.Join(os.Args[2:], " "))
	case "lock", "unlock":
		if len(os.Args) < 3 {
			fmt.Printf("usage: owltide %s <id>\n", os.Args[1])
			os.Exit(2)
		}
		lockCmd(os.Args[1], os.Args[2])
	case "changes":
		since := 24 * time.Hour
		if len(os.Args) >= 3 && strings.HasPrefix(os.Args[2], "--since") {
			if v, err := time.ParseDuration(strings.TrimPrefix(os.Args[2], "--since=")); err == nil {
				since = v
			}
		}
		changesCmd(since)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
}

// openStore initializes ~/.owltide layout and the auto-commit store.
func openStore() (*store.Store, error) {
	dir := platform.ConfigDir(platform.New().Home())
	if err := store.EnsureLayout(dir); err != nil {
		return nil, err
	}
	st, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	// First commit so rollbacks always have a parent.
	if err := st.EnsureInit(); err != nil {
		return nil, err
	}
	// Finish any rollback interrupted by a crash: locked files win.
	if err := st.RecoverStash(); err != nil {
		return nil, err
	}
	return st, nil
}

func lockCmd(op, id string) {
	st, err := openStore()
	must(err)
	if op == "lock" {
		err = st.Lock(id)
	} else {
		err = st.Unlock(id)
	}
	must(err)
	fmt.Println(op, id, "done")
}

func changesCmd(since time.Duration) {
	st, err := openStore()
	must(err)
	out, err := evolve.ChangesReport(st, since)
	must(err)
	fmt.Print(out)
}

// must exits on error; used only for trivial CLI setup.
func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// setup builds everything an agent run needs.
func setup() (agent.Options, func(), *subagent.Manager, error) {
	cfg, err := config.Load()
	if err != nil {
		return agent.Options{}, nil, nil, err
	}
	key := cfg.APIKey()
	if key == "" {
		return agent.Options{}, nil, nil, fmt.Errorf(
			"API key empty: set $%s", cfg.Model.APIKeyEnv)
	}
	p := platform.New()
	home := p.Home()
	workDir, _ := os.Getwd()

	var dl *decision.Logger
	if cfg.Logging.Decisions {
		dl, err = decision.Open(platform.ConfigDir(home) + "/logs")
		if err != nil {
			return agent.Options{}, nil, nil, err
		}
	}

	shell := tool.NewShell(p.Shell(), p.ShellFlag(), workDir, cfg.Budget.ToolTimeoutSec)
	reg := tool.NewRegistry()
	for _, t := range []tool.Tool{tool.ReadFile{}, &tool.WriteFile{}, &tool.EditFile{}, tool.ListDir{}, shell} {
		if err := reg.Register(t); err != nil {
			return agent.Options{}, nil, nil, err
		}
	}

	// Self-evolution: layout + auto-commit store + evolution tools.
	st, err := openStore()
	if err != nil {
		return agent.Options{}, nil, nil, err
	}
	// Dangerous actions (rm -rf, git push --force, locks.yaml writes)
	// ask the human at the terminal; the model can never self-approve.
	reg.SetConfirmer(confirmFromStdin)
	sessionID := time.Now().UTC().Format("20060102-150405")
	fmt.Println("session id:", sessionID)
	// Transaction-level backstop: any tool mutating protected files
	// (including run_shell tricks) gets rolled back and logged.
	gd := &guard.Guard{Root: st.Dir, St: st}

	// Inline & spill: oversized tool outputs go to disk, not context.
	sp, err := spill.New(st.Dir, sessionID, 0)
	if err != nil {
		return agent.Options{}, nil, nil, err
	}
	tool.SetSpillReader(func(ref string) (string, error) { return sp.Read(ref) })
	_ = spill.CleanupSession(st.Dir, sessionID)
	// Main conversation log: the single source of truth for what the
	// model saw (rebuild + future --resume read back from it).
	sessLog, err := sessionlog.Open(platform.ConfigDir(home)+"/sessions", sessionID)
	if err != nil {
		return agent.Options{}, nil, nil, err
	}
	if err := reg.Register(agent.TodoTool{Dir: sp.Dir}); err != nil {
		return agent.Options{}, nil, nil, err
	}

	// P2a: sub-agent manager + agent_* tools. Agents are loaded (and
	// built-ins seeded) from ~/.owltide/agents. Tool whitelists are
	// validated against the registry so a typo in agents/*.md fails
	// loudly at startup instead of at spawn time.
	if err := agents.SeedBuiltins(st.Dir); err != nil {
		return agent.Options{}, nil, nil, err
	}
	knownTools := map[string]bool{}
	for _, d := range reg.Definitions() {
		knownTools[d.Function.Name] = true
	}
	roleDefs, err := agents.Load(st.Dir, knownTools)
	if err != nil {
		return agent.Options{}, nil, nil, err
	}
	mgr, err := subagent.NewManager(subagent.ManagerOpts{
		Config: cfg, Client: model.NewClient(cfg.Model.BaseURL, key, nil),
		BaseReg: reg, Agents: roleDefs, Guard: gd, Spill: sp,
		Decisions: dl, SessionID: sessionID, WorkDir: workDir,
		TodoDir: sp.Dir, Loop: agent.LoopConfig{}, DecisionsOn: cfg.Logging.Decisions,
		Confirm:      confirmFromStdin,
		SessLog:      sessLog,
		SubLogDir:    platform.ConfigDir(home) + "/logs/sub",
		EventsLogDir: platform.ConfigDir(home) + "/logs",
		Events: func(event string, fields map[string]any) {
			// Audit sink: append one JSON line to logs/subagents.jsonl.
			subagent.AppendEvent(platform.ConfigDir(home)+"/logs", event, fields)
		},
	})
	if err != nil {
		return agent.Options{}, nil, nil, err
	}
	for _, t := range mgr.ManagerTools() {
		if err := reg.Register(t); err != nil {
			return agent.Options{}, nil, nil, err
		}
	}
	known := map[string]bool{}
	for _, d := range reg.Definitions() {
		known[d.Function.Name] = true
	}
	env := &evolve.Env{
		Root: st.Dir, St: st, SessionID: sessionID,
		Load: func(root string) error {
			_, err := config.Load()
			return err
		},
		Health: func(root string) error {
			_, err := config.Load()
			return err
		},
		KnownToolsSet: known,
	}
	if err := evolve.RegisterAll(reg, env); err != nil {
		return agent.Options{}, nil, nil, err
	}

	cleanup := func() { _ = sessLog.Close() }
	if dl != nil {
		prev := cleanup
		cleanup = func() { prev(); _ = dl.Close() }
	}

	return agent.Options{
		Config:    cfg,
		Client:    model.NewClient(cfg.Model.BaseURL, key, nil),
		Tools:     reg,
		Decisions: dl,
		SessionID: sessionID,
		WorkDir:   workDir,
		Guard:     gd,
		Spill:     sp,
		TodoDir:   sp.Dir,
		SessLog:   sessLog,
	}, cleanup, mgr, nil
}

// withSignal cancels the context on Ctrl-C.
func withSignal() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, func() { signal.Stop(ch); cancel() }
}

func runChat() {
	opts, cleanup, mgr, err := setup()
	must(err)
	defer cleanup()
	ctx, stop := withSignal()
	defer stop()

	a := agent.New(opts)
	mgr.SetParent(a)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	fmt.Println("OwlTide chat (empty line or Ctrl-D to exit)")
	for {
		fmt.Print("> ")
		if !sc.Scan() {
			break
		}
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		opts.OnDelta = func(s string) { fmt.Print(s) }
		a = agent.NewWithHistory(a, opts)
		reply, err := a.Run(ctx, text)
		if err != nil {
			fmt.Fprintln(os.Stderr, "\nerror:", err)
			continue
		}
		fmt.Println()
		_ = reply
	}
}

func runOneShot(task string) {
	opts, cleanup, mgr, err := setup()
	must(err)
	defer cleanup()
	ctx, stop := withSignal()
	defer stop()

	opts.OnDelta = func(s string) { fmt.Print(s) }
	a := agent.New(opts)
	mgr.SetParent(a)
	_, err = a.Run(ctx, task)
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// confirmFromStdin asks the user a yes/no question on the terminal.
func confirmFromStdin(prompt string) bool {
	fmt.Printf("\n%s [y/N] > ", prompt)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes"
}
