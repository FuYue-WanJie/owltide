package tool

// Dangerous-command detection. Commands are parsed with a real shell
// grammar (mvdan.cc/sh, pure Go, no cgo). Words are evaluated
// statically on the syntax tree (Lit + SglQuoted + literal-only
// DblQuoted); any word that cannot be evaluated statically (ParamExp,
// CmdSubst, arithmetic, ...) marks the whole command dangerous — fail
// closed. Pipelines, ;, &&, subshells and command substitutions are
// walked; wrapper binaries (sudo, nohup, env, toybox, ...) are peeled
// before matching the rule table.
//
// This is a speed bump, not a sandbox: quoted obfuscation of command
// *names* is covered by static evaluation, but scripts that compute
// their arguments at runtime, functions, aliases and non-shell
// interpreters (python -c, perl -e) are known limitations. The
// transaction-level guard (core/guard: protected-file rollback) is
// the real backstop, and user confirmation is the final authority.

import (
	"fmt"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Confirmer asks the user a yes/no question.
type Confirmer func(prompt string) bool

// DetailConfirmer is the structured confirmation callback: kind and
// detail (command text or path) travel with the prompt so the caller
// can apply session-allow policy per exact command. When set it takes
// precedence over Confirmer.
type DetailConfirmer func(prompt, kind, detail string) bool

// detailConfirmerHolder is implemented by tools that can carry the
// structured confirmation callback.
type detailConfirmerHolder interface {
	setDetailConfirmer(DetailConfirmer)
}

// confirmerHolder is implemented by tools that can ask the user.
type confirmerHolder interface {
	setConfirmer(Confirmer)
}

// isDangerousCommand parses the command and checks every sub-command
// against the rule table. Unparseable input is dangerous by default.
func isDangerousCommand(cmd string) bool {
	prog, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return true // fail closed on syntax we cannot model
	}
	danger := false
	syntax.Walk(prog, func(node syntax.Node) bool {
		if danger {
			return false
		}
		if r, ok := node.(*syntax.Redirect); ok {
			// > locks.yaml / > config.yaml etc: any redirect into a
			// protected file is dangerous by itself.
			if r.Word != nil {
				if target, err := evalWord(r.Word); err != nil {
					danger = true
					return false
				} else if isLocksFile(target) || strings.HasSuffix(target, "/locks.yaml") {
					danger = true
					return false
				}
			}
			return true
		}
		if c, ok := node.(*syntax.CallExpr); ok {
			ev := evalWords(c.Args)
			if len(ev.words) == 0 {
				return true
			}
			// Peel wrapper binaries first; note whether peeling saw
			// dynamic words (the wrapped command may be dynamic).
			rest, peeledDyn := peelWrappersEv(ev)
			if len(rest.words) == 0 {
				return true
			}
			// Rule 1: an unevaluable command NAME is dangerous.
			if rest.dynami[0] {
				danger = true
				return false
			}
			name := rest.words[0]
			// Rule 2: risky command + dynamic ARGUMENTS is dangerous.
			if isRiskyCommand(name) && rest.dynamicArgs() {
				danger = true
				return false
			}
			if _ = peeledDyn; isRiskyCommand(name) && peeledDyn {
				danger = true
				return false
			}
			// Rule 3: other commands may carry dynamic arguments —
			// allowed; $(...) subcommands were already walked and
			// checked by this very visitor.
			if commandIsDangerous(rest.words) {
				danger = true
				return false
			}
			// sh -c 'script' / eval 'cmd': re-parse the body.
			if body, ok := wrappedBody(rest.words); ok {
				if isDangerousCommand(body) {
					danger = true
					return false
				}
			}
		}
		return true
	})
	return danger
}

// evalResult carries a statically-evaluated word list plus which
// positions could not be evaluated (dynamic words like $VAR, $(...)).
type evalResult struct {
	words  []string // evaluated text; dynamic positions hold ""
	dynami []bool   // true where the word is not statically evaluable
}

// dynamic reports whether any word is unevaluable.
func (e evalResult) dynamic() bool {
	for _, d := range e.dynami {
		if d {
			return true
		}
	}
	return false
}

// dynamicArgs reports whether any word AFTER the command name is
// unevaluable.
func (e evalResult) dynamicArgs() bool {
	for _, d := range e.dynami[minInt(1, len(e.dynami)):] {
		if d {
			return true
		}
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// slice drops the first n words.
func (e evalResult) slice(n int) evalResult {
	if n >= len(e.words) {
		return evalResult{}
	}
	return evalResult{words: e.words[n:], dynami: e.dynami[n:]}
}

// evalWords statically evaluates a word list; unevaluable words are
// marked, not fatal (see isDangerousCommand for the policy).
func evalWords(args []*syntax.Word) evalResult {
	res := evalResult{
		words:  make([]string, len(args)),
		dynami: make([]bool, len(args)),
	}
	for i, w := range args {
		if s, err := evalWord(w); err == nil {
			res.words[i] = s
		} else {
			res.dynami[i] = true
		}
	}
	return res
}

// evalWord concatenates statically known parts of a word.
func evalWord(w *syntax.Word) (string, error) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, dp := range p.Parts {
				if lit, ok := dp.(*syntax.Lit); ok {
					b.WriteString(lit.Value)
				} else {
					return "", fmt.Errorf("non-literal in double quotes")
				}
			}
		default:
			// ParamExp, CmdSubst, ArithmExp, ProcSubst, ExtGlob...
			return "", fmt.Errorf("word is not statically evaluable")
		}
	}
	return b.String(), nil
}

// wrapperWord reports whether w is a wrapper binary that can be peeled
// before scanning the wrapped command.
func wrapperWord(w string) bool {
	base := filepath.Base(w)
	switch base {
	case "sudo", "doas", "nohup", "timeout", "nice", "ionice",
		"stdbuf", "time", "command", "exec", "env", "busybox", "toybox":
		return true
	}
	return false
}

// isRiskyCommand reports whether a command name carries enough
// destructive potential that DYNAMIC arguments around it must be
// treated as dangerous (we cannot see what they expand to).
func isRiskyCommand(name string) bool {
	base := filepath.Base(strings.TrimPrefix(name, "\\"))
	switch base {
	case "rm", "find", "git", "dd", "chmod", "chown", "mkfs",
		"sh", "bash", "dash", "ash", "zsh", "ksh", "eval", "xargs",
		"sudo", "doas", "nohup", "timeout", "env", "cp", "mv", "ln",
		"truncate", "shred", "shutdown", "reboot", "poweroff", "halt",
		"mount", "umount", "fdisk", "parted", "kill", "pkill", "killall",
		"chattr", "setfacl", "tee":
		return true
	}
	return strings.HasPrefix(base, "mkfs.")
}

// peelWrappersEv strips wrapper binaries and their own options from an
// evaluated word list, returning the innermost command. Dynamic words
// survive peeling and keep their flags.
func peelWrappersEv(ev evalResult) (evalResult, bool) {
	peeled := false
	for len(ev.words) > 0 && ev.dynami[0] == false && wrapperWord(ev.words[0]) {
		peeled = true
		base := filepath.Base(ev.words[0])
		ev = ev.slice(1)
		// normalize struct literal
		switch base {
		case "env":
			for len(ev.words) > 0 {
				w, dyn := ev.words[0], ev.dynami[0]
				if dyn {
					break // cannot reason past a dynamic word
				}
				if w == "-i" || w == "--ignore-environment" {
					ev = ev.slice(1)
					continue
				}
				if w == "-u" && len(ev.words) > 1 {
					ev = ev.slice(2)
					continue
				}
				if strings.HasPrefix(w, "-") {
					ev = ev.slice(1)
					continue
				}
				if strings.Contains(w, "=") {
					ev = ev.slice(1)
					continue
				}
				break
			}
		case "timeout":
			for len(ev.words) > 0 && !ev.dynami[0] && strings.HasPrefix(ev.words[0], "-") {
				w := ev.words[0]
				ev = ev.slice(1)
				if (w == "-k" || w == "--kill-after") && len(ev.words) > 0 {
					ev = ev.slice(1)
				}
			}
			if len(ev.words) > 0 {
				ev = ev.slice(1) // duration
			}
		case "nohup", "nice", "ionice", "stdbuf", "time", "command", "doas":
			for len(ev.words) > 0 && !ev.dynami[0] && strings.HasPrefix(ev.words[0], "-") {
				w := ev.words[0]
				ev = ev.slice(1)
				if w == "-n" && base == "nice" && len(ev.words) > 0 {
					ev = ev.slice(1)
				}
			}
		case "sudo":
			for len(ev.words) > 0 {
				w, dyn := ev.words[0], ev.dynami[0]
				if dyn {
					break
				}
				if w == "--" {
					ev = ev.slice(1)
					break
				}
				if !strings.HasPrefix(w, "-") {
					break
				}
				ev = ev.slice(1)
				if (w == "-u" || w == "-g" || w == "--user" || w == "--group") && len(ev.words) > 0 {
					ev = ev.slice(1)
				}
			}
		}
	}
	// busybox/toybox applet drop
	if len(ev.words) > 0 && !ev.dynami[0] {
		base := filepath.Base(ev.words[0])
		if base == "busybox" || base == "toybox" {
			if len(ev.words) > 1 {
				ev = ev.slice(1)
				peeled = true
			}
		}
	}
	return ev, peeled
}

// peelWrappers strips wrapper binaries and their own options from the
// word list, returning the innermost command. busybox/toybox rewrite
// their applet as the first inner word.
func peelWrappers(words []string) ([]string, bool) {
	peeled := false
	for len(words) > 0 && wrapperWord(words[0]) {
		peeled = true
		base := filepath.Base(words[0])
		words = words[1:]
		switch base {
		case "env":
			// skip -i/-u... and VAR=x assignments
			for len(words) > 0 {
				w := words[0]
				if w == "-i" || w == "-u" || w == "--ignore-environment" {
					words = words[1:]
					if w == "-u" && len(words) > 0 {
						words = words[1:] // -u takes a name
					}
					continue
				}
				if strings.HasPrefix(w, "-") {
					words = words[1:]
					continue
				}
				if strings.Contains(w, "=") { // VAR=x
					words = words[1:]
					continue
				}
				break
			}
		case "timeout":
			// skip duration and its flags: timeout 10s cmd / timeout -k 5 10s cmd
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				w := words[0]
				words = words[1:]
				if w == "-k" || w == "--kill-after" && len(words) > 0 {
					words = words[1:]
				}
			}
			if len(words) > 0 {
				words = words[1:] // duration
			}
		case "nohup", "nice", "ionice", "stdbuf", "time", "command", "doas":
			// skip own flags (e.g. nice -n 5, stdbuf -oL)
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				w := words[0]
				words = words[1:]
				if w == "-n" && base == "nice" && len(words) > 0 {
					words = words[1:]
				}
			}
		case "sudo":
			// skip options; -u USER / -g GROUP take a value
			for len(words) > 0 {
				w := words[0]
				if w == "--" {
					words = words[1:]
					break
				}
				if !strings.HasPrefix(w, "-") {
					break
				}
				words = words[1:]
				if (w == "-u" || w == "-g" || w == "--user" || w == "--group") && len(words) > 0 {
					words = words[1:]
				}
			}
		}
	}
	// busybox/toybox applet: drop the applet name so rules match.
	if len(words) > 0 {
		base := filepath.Base(words[0])
		if base == "busybox" || base == "toybox" {
			if len(words) > 1 {
				words = words[1:]
				peeled = true
			}
		}
	}
	return words, peeled
}

// wrappedBody extracts the re-parsable body of `sh -c 'script'` /
// `eval ...` word lists after peeling.
func wrappedBody(words []string) (string, bool) {
	if len(words) == 0 {
		return "", false
	}
	base := filepath.Base(words[0])
	switch base {
	case "sh", "bash", "dash", "ash", "zsh", "ksh":
		for i := 1; i+1 < len(words); i++ {
			if words[i] == "-c" {
				return words[i+1], true
			}
		}
	case "eval":
		if len(words) > 1 {
			return strings.Join(words[1:], " "), true
		}
	}
	return "", false
}

// commandIsDangerous matches one peeled simple command (no operators)
// against the rule table.
func commandIsDangerous(words []string) bool {
	if len(words) == 0 {
		return false
	}
	// Recurse through wrappers so "sudo -u root rm -rf /" is matched.
	if len(words) > 1 && wrapperWord(words[0]) {
		rest, _ := peelWrappers(words)
		if len(rest) < len(words) {
			if commandIsDangerous(rest) {
				return true
			}
		}
	}
	// Unescape backslash-escaped command names: \rm == rm.
	name := strings.TrimPrefix(words[0], "\\")
	base := filepath.Base(name)

	recursive := func() bool {
		for _, w := range words[1:] {
			if w == "-r" || w == "-R" || w == "--recursive" ||
				(strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.ContainsAny(w[1:], "rR")) {
				return true
			}
		}
		return false
	}
	// git: skip global options (-C dir, -c k=v, --git-dir, --work-tree)
	// before locating the subcommand.
	if base == "git" {
		i := 1
		for i < len(words) {
			w := words[i]
			if w == "-C" || w == "--git-dir" || w == "--work-tree" {
				i += 2
				continue
			}
			if w == "-c" {
				i += 2
				continue
			}
			if strings.HasPrefix(w, "-") {
				i++
				continue
			}
			break
		}
		sub := wordAt(words, i)
		switch sub {
		case "push":
			for _, w := range words[i+1:] {
				if w == "--force" || w == "--force-with-lease" || w == "-f" || w == "--delete" {
					return true
				}
				if strings.HasPrefix(w, "+") && len(w) > 1 {
					return true // refspec +main
				}
			}
		case "reset":
			if hasFlag(words[i:], "--hard") {
				return true
			}
		case "clean":
			if hasFlag(words[i:], "-f", "--force", "-d", "-x", "-X", "-fd", "-fdx", "-xdf") {
				return true
			}
		case "branch":
			if hasFlag(words[i:], "-D") {
				return true
			}
		case "filter-branch":
			return true
		}
		return false
	}

	// xargs: scan the wrapped command.
	if base == "xargs" && len(words) > 1 {
		return commandIsDangerous(words[1:])
	}

	switch {
	// Any command whose arguments or redirections touch the lock
	// registry needs user sign-off (echo/sed/cp/mv/tee > locks.yaml).
	case touchesLocksFile(words):
		return true
	case base == "rm" && recursive():
		return true // rm -r anything is destructive; -f implied risk
	case base == "find":
		for _, w := range words[1:] {
			if w == "-delete" || w == "-exec" || w == "-execdir" || w == "-ok" || w == "-okdir" {
				return true
			}
		}
		return false
	case base == "mkfs" || strings.HasPrefix(base, "mkfs."):
		return true
	case base == "dd":
		for _, w := range words[1:] {
			if strings.HasPrefix(w, "of=/dev/") {
				return true
			}
		}
		return false
	case base == "chmod" && hasFlag(words, "-R", "--recursive") && hasFlag(words, "777"):
		return true
	case base == "shutdown" || base == "reboot" || base == "poweroff" || base == "halt" || base == "init":
		return true
	}
	return false
}

// touchesLocksFile reports whether the command references locks.yaml
// (as an argument, redirect target, or here-doc tag). Redirections are
// part of the Stmt, not the CallExpr, so the caller pre-filters; here
// we check argument words only.
func touchesLocksFile(words []string) bool {
	for _, w := range words {
		if isLocksFile(w) {
			return true
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

func hasFlag(words []string, flags ...string) bool {
	for _, w := range words {
		for _, f := range flags {
			if w == f {
				return true
			}
			if strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w[1:], strings.TrimPrefix(f, "-")) {
				return true
			}
		}
	}
	return false
}

// isLocksFile reports whether a path targets the lock registry.
func isLocksFile(path string) bool {
	return filepath.Base(path) == "locks.yaml"
}

// confirmDanger routes a dangerous action through the confirmer.
// The returned error text is stable so logs are greppable.
func confirmDanger(c Confirmer, dc DetailConfirmer, kind, detail string) error {
	prompt := fmt.Sprintf("DANGEROUS %s: %s — allow?", kind, detail)
	if dc != nil {
		if !dc(prompt, kind, detail) {
			return fmt.Errorf("%s rejected by user", prompt)
		}
		return nil
	}
	if c == nil {
		return fmt.Errorf("%s requires user confirmation (no interactive session); rejected", prompt)
	}
	if !c(prompt) {
		return fmt.Errorf("%s rejected by user", prompt)
	}
	return nil
}
