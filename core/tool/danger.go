package tool

// Dangerous-command detection. Commands are parsed with a real shell
// grammar (mvdan.cc/sh, pure Go, no cgo) and every sub-command in the
// tree (pipelines, ;, &&, nested -c/eval wrappers) is checked against
// a rule table. Parse failures are treated as dangerous (fail closed).
//
// This is a speed bump, not a sandbox: variable expansion (unquoted
// $VAR), aliases, functions, and deliberate obfuscation cannot be
// exhaustively covered — rules below are a best-effort table adapted
// from Coren's pkg/risk rule list. The transaction-level guard
// (core/guard: protected-file hash rollback) is the real backstop,
// and user confirmation is the final authority.

import (
	"fmt"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Confirmer asks the user a yes/no question.
type Confirmer func(prompt string) bool

// confirmerHolder is implemented by tools that can ask the user.
type confirmerHolder interface {
	setConfirmer(Confirmer)
}

// isDangerousCommand parses the command and checks every sub-command
// against the rule table. Unparseable input is dangerous by default.
func isDangerousCommand(cmd string) bool {
	src := cmd
	prog, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return true // fail closed on syntax we cannot model
	}
	danger := false
	syntax.Walk(prog, func(node syntax.Node) bool {
		if danger {
			return false
		}
		switch n := node.(type) {
		case *syntax.CallExpr:
			if commandIsDangerous(callExprText(src, n)) {
				danger = true
				return false
			}
			// Recurse into wrapped commands: sh -c '...', bash -c, eval.
			for _, wrapped := range wrappedBodies(src, n) {
				if isDangerousCommand(wrapped) {
					danger = true
					return false
				}
			}
		}
		return true
	})
	return danger
}

// wordText returns the raw source text of a word (quotes included),
// falling back to Lit for constructed words.
func wordText(src string, w *syntax.Word) string {
	p, e := int(w.Pos().Offset()), int(w.End().Offset())
	if p >= 0 && e <= len(src) && p < e {
		return src[p:e]
	}
	return w.Lit()
}

// callExprText renders a call back to its words (raw source text).
func callExprText(src string, c *syntax.CallExpr) string {
	var parts []string
	for _, w := range c.Args {
		parts = append(parts, wordText(src, w))
	}
	return strings.Join(parts, " ")
}

// wrappedBodies extracts the command strings passed to sh -c / bash -c
// / eval wrappers so nested commands are scanned. Raw source text keeps
// quotes; unquoting is not needed because we re-parse after stripping.
func wrappedBodies(src string, c *syntax.CallExpr) []string {
	if len(c.Args) == 0 {
		return nil
	}
	base := filepath.Base(c.Args[0].Lit())
	var out []string
	switch base {
	case "sh", "bash", "dash", "ash", "zsh", "ksh":
		for i := 1; i+1 < len(c.Args); i++ {
			if c.Args[i].Lit() == "-c" {
				out = append(out, unquote(wordText(src, c.Args[i+1])))
			}
		}
	case "eval":
		var words []string
		for _, a := range c.Args[1:] {
			words = append(words, unquote(wordText(src, a)))
		}
		if len(words) > 0 {
			out = append(out, strings.Join(words, " "))
		}
	}
	return out
}

// unquote strips one layer of shell quoting so the body can be re-parsed.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// rule is one dangerous-command signature from the risk table
// (adapted from Coren pkg/risk rules — rules only, no plugin structure).
type rule struct {
	match func(words []string) bool
	why   string
}

// hasFlag reports whether any word equals or starts with (for joined
// short flags like -rf) the given flag.
func hasFlag(words []string, flags ...string) bool {
	for _, w := range words[1:] {
		for _, f := range flags {
			if w == f || (strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w[1:], f[1:])) {
				return true
			}
		}
	}
	return false
}

func wordAt(words []string, i int) string {
	if i < len(words) {
		return words[i]
	}
	return ""
}

// commandIsDangerous matches one simple command (no operators) against
// the rule table.
func commandIsDangerous(text string) bool {
	words := strings.Fields(text)
	if len(words) == 0 {
		return false
	}
	base := filepath.Base(wordAt(words, 0))
	// sudo/doas prefix: check the wrapped command instead.
	if base == "sudo" || base == "doas" {
		if len(words) > 1 {
			// skip flag arguments like -u user
			rest := words[1:]
			for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
				rest = rest[1:]
				// -u takes a value
				if rest[0] != "" && rest[0][0] == '-' && len(rest) > 1 && strings.HasPrefix(rest[0], "-u") == false {
				}
			}
			return commandIsDangerous(strings.Join(rest, " "))
		}
		return false
	}

	recursive := func() bool {
		for _, w := range words[1:] {
			if w == "-r" || w == "-R" || w == "--recursive" ||
				(strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.ContainsAny(w[1:], "rR")) {
				return true
			}
		}
		return false
	}
	force := func() bool {
		for _, w := range words[1:] {
			if w == "-f" || w == "--force" ||
				(strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w[1:], "f")) {
				return true
			}
		}
		return false
	}

	// xargs passes the wrapped command through; scan it recursively.
	if base == "xargs" && len(words) > 1 {
		return commandIsDangerous(strings.Join(words[1:], " "))
	}

	switch {
	// rm -r -f / -rf / -fr / --recursive --force
	case base == "rm" && recursive() && force():
		return true
	// find ... -delete or -exec rm
	case base == "find":
		for _, w := range words {
			if w == "-delete" || w == "-execdir" {
				return true
			}
			if w == "-exec" {
				return true
			}
		}
		return false
	// git push variants
	case base == "git" && wordAt(words, 1) == "push":
		tail := words[2:]
		for _, w := range tail {
			if w == "--force" || w == "--force-with-lease" || w == "-f" {
				return true
			}
			// refspec form: git push origin +main
			if strings.HasPrefix(w, "+") && strings.Contains(w, "main") || strings.HasPrefix(w, "refs/heads/+") || (strings.HasPrefix(w, "+") && w != "+") {
				return true
			}
		}
		return false
	case base == "git" && wordAt(words, 1) == "reset" && hasFlag(words, "--hard"):
		return true
	case base == "git" && (wordAt(words, 1) == "clean"):
		if hasFlag(words, "-f", "--force") || hasFlag(words, "-d", "-x", "-fd", "-fdx", "-xdf") {
			return true
		}
		return false
	case base == "git" && wordAt(words, 1) == "branch" && hasFlag(words, "-D"):
		return true
	case base == "git" && wordAt(words, 1) == "filter-branch":
		return true
	case base == "git" && wordAt(words, 1) == "push" && hasFlag(words, "--delete"):
		return true
	// destructive fs / system
	case base == "mkfs" || strings.HasPrefix(base, "mkfs."):
		return true
	case base == "dd":
		for _, w := range words {
			if strings.HasPrefix(w, "of=/dev/") {
				return true
			}
		}
		return false
	case base == "chmod" && hasFlag(words, "-R", "--recursive") && hasFlag(words, "777"):
		return true
	case base == "shutdown" || base == "reboot" || base == "poweroff" || base == "halt" || base == "init":
		return true
	case base == "rm" && recursive(): // rm -r without -f is still destructive here
		return true
	}
	return false
}

// isLocksFile reports whether a path targets the lock registry.
func isLocksFile(path string) bool {
	return filepath.Base(path) == "locks.yaml"
}

// confirmDanger routes a dangerous action through the confirmer.
// The returned error text is stable so logs are greppable.
func confirmDanger(c Confirmer, kind, detail string) error {
	prompt := fmt.Sprintf("DANGEROUS %s: %s — allow?", kind, detail)
	if c == nil {
		return fmt.Errorf("%s requires user confirmation (no interactive session); rejected", prompt)
	}
	if !c(prompt) {
		return fmt.Errorf("%s rejected by user", prompt)
	}
	return nil
}
