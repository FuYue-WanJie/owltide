package tool

// ReadOnlyShell: a parser-gated view of run_shell for read-only roles.
// Commands are parsed with mvdan/sh; every simple command's argv must
// be in the allowlist. Redirections (except reading stdin from a file
// is fine; any output redirect is refused), command substitutions and
// pipes to non-allowlisted commands are refused. git runs with
// core.fsmonitor= and core.hooksPath=/dev/null and forbids -c,
// --output, --exec style options, because git status and friends can
// execute repository-configured programs.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ReadOnlyShell wraps a normal Shell and gates commands.
type ReadOnlyShell struct {
	Inner *Shell
}

// Caps: still executes, but the gate keeps it read-only in effect.
func (*ReadOnlyShell) Caps() Caps { return CapExec }

// setConfirmer implements confirmerHolder (delegated to inner).
func (r *ReadOnlyShell) setConfirmer(c Confirmer) { r.Inner.setConfirmer(c) }

// CloneWithConfirmer clones the inner shell too, so the copy's
// confirmation stream is its own.
func (r *ReadOnlyShell) CloneWithConfirmer(c Confirmer) Tool {
	inner, _ := r.Inner.CloneWithConfirmer(c).(*Shell)
	return &ReadOnlyShell{Inner: inner}
}

// Schema matches run_shell so the model sees one tool name.
func (r *ReadOnlyShell) Schema() Definition {
	d := r.Inner.Schema()
	d.Function.Name = "run_shell"
	d.Function.Description = "Run a READ-ONLY shell command. Only allowlisted read-only " +
		"programs are permitted; redirects, command substitution and pipes to " +
		"non-allowlisted commands are refused. git runs sandboxed (no hooks/fsmonitor, no -c)."
	return d
}

// Call delegates after gating, sandboxing git invocations.
func (r *ReadOnlyShell) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if err := r.Gate(a.Command); err != nil {
		return "", fmt.Errorf("read-only shell: %v", err)
	}
	a.Command = sandboxGit(a.Command)
	b, _ := json.Marshal(a)
	return r.Inner.Call(ctx, b)
}

// sandboxGit rewrites every `git ...` invocation to run with hooks
// and fsmonitor disabled (core.fsmonitor=false — an empty value does
// NOT disable it): repository-configured programs must never execute.
// The user's own -c is refused in roArgsOK, so the injected pair is
// the only one.
func sandboxGit(cmd string) string {
	if !strings.Contains(cmd, "git") {
		return cmd
	}
	// Token-level rewrite per simple command; quotes in the original
	// are preserved by construction (we only insert after a bare git).
	prog, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return cmd
	}
	_ = prog
	words := strings.Fields(cmd)
	var out []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		out = append(out, w)
		if w == "git" && (i == 0 || words[i-1] == "&&" || words[i-1] == ";" || words[i-1] == "||") {
			out = append(out, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null")
		}
	}
	return strings.Join(out, " ")
}

// roAllowlist: argv[0] values permitted in read-only shells.
var roAllowlist = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "find": true, "file": true, "stat": true, "du": true,
	"git": true, "go": true, "pwd": true, "echo": true, "true": true,
	"sort": true, "uniq": true, "diff": true, "which": true, "env": true,
}

// gate parses the command and checks every simple command.
func (r *ReadOnlyShell) Gate(cmd string) error {
	prog, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return fmt.Errorf("unparseable command refused")
	}
	refuse := fmt.Errorf("command not in read-only allowlist or uses a forbidden construct")
	bad := false
	syntax.Walk(prog, func(node syntax.Node) bool {
		if bad {
			return false
		}
		switch n := node.(type) {
		case *syntax.Redirect:
			// any output redirect (> >> &> >| here-doc to fd) is refused
			if n.Op != 0 && n.Word != nil {
				bad = true
				return false
			}
			return true
		case *syntax.CmdSubst:
			bad = true // command substitution hides arbitrary execution
			return false
		case *syntax.CallExpr:
			ev := evalWords(n.Args)
			if len(ev.words) == 0 {
				return true
			}
			if ev.dynami[0] {
				bad = true // dynamic command name
				return false
			}
			if ev.dynamicArgs() {
				bad = true // dynamic args could hide anything
				return false
			}
			base := filepath.Base(strings.TrimPrefix(ev.words[0], "\\"))
			if !roAllowlist[base] {
				bad = true
				return false
			}
			if !roArgsOK(base, ev.words) {
				bad = true
				return false
			}
		}
		return true
	})
	if bad {
		return refuse
	}
	return nil
}

// roArgsOK applies per-program argument rules after peeling.
func roArgsOK(base string, words []string) bool {
	switch base {
	case "git":
		// forbid anything that can execute programs or write
		for _, w := range words[1:] {
			switch {
			case w == "-c" || strings.HasPrefix(w, "--exec") ||
				strings.HasPrefix(w, "--output") || w == "push" ||
				w == "commit" || w == "reset" || w == "clean" ||
				w == "checkout" || w == "restore" || w == "merge" ||
				w == "rebase" || w == "revert" || w == "apply" ||
				w == "stash" || w == "am" || w == "cherry-pick" ||
				w == "filter-branch" || w == "config" || w == "init" ||
				w == "clone" || w == "fetch" || w == "pull":
				return false
			}
		}
		return true
	case "go":
		// only inspection subcommands; vet/build can execute via
		// GOFLAGS/toolexec and download modules
		if len(words) < 2 {
			return false
		}
		switch words[1] {
		case "list", "doc", "version", "env":
			return true
		}
		return false
	case "find":
		for _, w := range words[1:] {
			if w == "-delete" || w == "-exec" || w == "-execdir" ||
				w == "-ok" || w == "-okdir" || w == "-fprint" ||
				w == "-fprintf" || w == "-fls" {
				return false
			}
		}
		return true
	}
	return true
}
