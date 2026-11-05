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
	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/decision"
	"github.com/FuYue-WanJie/owltide/core/evolve"
	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/platform"
	"github.com/FuYue-WanJie/owltide/core/store"
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
		must(config.WriteDefault(config.Path()))
		must(store.EnsureLayout(platform.ConfigDir(platform.New().Home())))
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
	return store.Open(dir)
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
func setup() (agent.Options, func(), error) {
	cfg, err := config.Load()
	if err != nil {
		return agent.Options{}, nil, err
	}
	key := cfg.APIKey()
	if key == "" {
		return agent.Options{}, nil, fmt.Errorf(
			"API key empty: set $%s", cfg.Model.APIKeyEnv)
	}
	p := platform.New()
	home := p.Home()
	workDir, _ := os.Getwd()

	var dl *decision.Logger
	if cfg.Logging.Decisions {
		dl, err = decision.Open(platform.ConfigDir(home) + "/logs")
		if err != nil {
			return agent.Options{}, nil, err
		}
	}

	shell := tool.NewShell(p.Shell(), p.ShellFlag(), workDir, cfg.Budget.ToolTimeoutSec)
	reg := tool.NewRegistry()
	for _, t := range []tool.Tool{tool.ReadFile{}, tool.WriteFile{}, tool.EditFile{}, tool.ListDir{}, shell} {
		if err := reg.Register(t); err != nil {
			return agent.Options{}, nil, err
		}
	}

	// Self-evolution: layout + auto-commit store + evolution tools.
	st, err := openStore()
	if err != nil {
		return agent.Options{}, nil, err
	}
	sessionID := time.Now().UTC().Format("20060102-150405")
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
		return agent.Options{}, nil, err
	}

	cleanup := func() {}
	if dl != nil {
		cleanup = func() { _ = dl.Close() }
	}

	return agent.Options{
		Config:    cfg,
		Client:    model.NewClient(cfg.Model.BaseURL, key, nil),
		Tools:     reg,
		Decisions: dl,
		SessionID: sessionID,
		WorkDir:   workDir,
	}, cleanup, nil
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
	opts, cleanup, err := setup()
	must(err)
	defer cleanup()
	ctx, stop := withSignal()
	defer stop()

	a := agent.New(opts)
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
	opts, cleanup, err := setup()
	must(err)
	defer cleanup()
	ctx, stop := withSignal()
	defer stop()

	opts.OnDelta = func(s string) { fmt.Print(s) }
	a := agent.New(opts)
	_, err = a.Run(ctx, task)
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
