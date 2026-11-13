package main

// Permission-service CLI endpoint (P2b-M14): the terminal is one
// answering endpoint ("cli") in the perm table. Prompt text and
// interaction are byte-identical to the pre-perm implementation; the
// state machine owns timeouts, first-come-first-served across
// endpoints (CLI / serve / future TUI) and the audit trail.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/FuYue-WanJie/owltide/core/agent"
	"github.com/FuYue-WanJie/owltide/internal/perm"
)

// promptAsk reads the terminal (same format as the pre-perm
// askFromStdin).
func promptAsk(question string, choices []string) (string, error) {
	fmt.Printf("\n%s\n", question)
	if len(choices) > 0 {
		for i, c := range choices {
			fmt.Printf("  %d) %s\n", i+1, c)
		}
		fmt.Println("（输入选项序号或自由文本，回车提交）")
	} else {
		fmt.Print("> ")
	}
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", fmt.Errorf("no answer provided")
	}
	ans := strings.TrimSpace(sc.Text())
	// Numeric choice maps back to the option text.
	if n := 0; len(choices) > 0 {
		if _, err := fmt.Sscanf(ans, "%d", &n); err == nil && n >= 1 && n <= len(choices) {
			return choices[n-1], nil
		}
	}
	return ans, nil
}

// promptConfirm asks a yes/no question on the terminal (same format as
// the pre-perm confirmFromStdin).
func promptConfirm(prompt string) bool {
	fmt.Printf("\n%s [y/N] > ", prompt)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes"
}

// waitForTerminal waits for the request's terminal state; a short
// grace period covers the (rare) race where another endpoint answered
// a moment ago and Wait misses the close.
func waitForTerminal(svc *perm.Service, id string) (perm.Outcome, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return svc.Wait(ctx, id)
}

// askViaPerm runs ask_user through the permission service.
//
//	approved  -> the answer text (ours, or another endpoint's Response)
//	timeout   -> agent.AskTimeoutMessage (documented semantic, no mark)
//	denied    -> the legacy error (EOF: "no answer provided")
//	cancelled -> session ended
func askViaPerm(svc *perm.Service, sessionID string,
	askFn func(string, []string) (string, error), question string, choices []string,
) (string, error) {
	req, err := svc.Ask(perm.KindAskUser, perm.Source{Agent: "main", Session: sessionID},
		map[string]any{"question": question, "choices": choices})
	if err != nil {
		return "", err
	}
	ans, aerr := askFn(question, choices)
	if aerr == nil {
		_, _ = svc.Respond(req.ID, ans, "cli")
	} else {
		_, _ = svc.Answer(req.ID, perm.StateDenied, "cli")
	}
	out, werr := waitForTerminal(svc, req.ID)
	if werr != nil {
		return "", fmt.Errorf("ask_user: %w", werr)
	}
	switch out.Request.State {
	case perm.StateApproved:
		if out.Request.Response != "" {
			return out.Request.Response, nil
		}
		return ans, nil
	case perm.StateTimeout:
		return agent.AskTimeoutMessage, nil
	case perm.StateCancelled:
		return "", fmt.Errorf("ask_user: 会话已结束")
	default:
		if aerr != nil {
			return "", fmt.Errorf("ask_user: %w", aerr)
		}
		return "", fmt.Errorf("ask_user: no answer provided")
	}
}

// confirmViaPerm routes a dangerous-action confirmation through the
// permission service. Timeout = deny; another endpoint may win the
// race and its terminal state is honored (first come first served).
func confirmViaPerm(svc *perm.Service, src perm.Source,
	confirmFn func(string) bool, prompt string,
) bool {
	req, err := svc.Ask(perm.KindApproval, src, map[string]any{"prompt": prompt})
	if err != nil {
		return false
	}
	ok := confirmFn(prompt)
	state := perm.StateDenied
	if ok {
		state = perm.StateApproved
	}
	_, _ = svc.Answer(req.ID, state, "cli")
	out, werr := waitForTerminal(svc, req.ID)
	if werr != nil {
		return false
	}
	return out.Request.State == perm.StateApproved
}
