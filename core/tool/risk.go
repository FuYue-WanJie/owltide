package tool

// Risk set and command signatures (P2b-M15).
//
// IsRiskCommand is the NON-overridable risk set: destructive or
// privilege-related operations that always require individual user
// confirmation, even when a session allow exists. The broader
// isDangerousCommand stays the confirmation trigger; commands outside
// the risk set may be pre-approved for the rest of the session via a
// session allow (perm.Service).
//
// CommandSignature produces the exact argv signature a session allow
// is keyed on: every simple command's statically-evaluated argv,
// canonicalized; anything dynamic falls back to the raw command text
// (fail closed — dynamic commands are never allowlisted).

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// IsRiskCommand reports whether cmd belongs to the non-overridable
// risk set: recursive/file deletion, force git operations, protected-
// file redirection, privilege escalation, disk/format/power/process
// operations, and anything the static evaluator cannot model. Fail
// closed: unparseable or dynamic input is risk.
func IsRiskCommand(cmd string) bool {
	prog, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return true // fail closed
	}
	risk := false
	syntax.Walk(prog, func(node syntax.Node) bool {
		if risk {
			return false
		}
		switch n := node.(type) {
		case *syntax.Redirect:
			if n.Word != nil {
				if target, err := evalWord(n.Word); err != nil {
					risk = true
					return false
				} else if isProtectedTarget(target) {
					risk = true
					return false
				}
			}
		case *syntax.CallExpr:
			ev := evalWords(n.Args)
			if len(ev.words) == 0 {
				return true
			}
			// Privilege escalation is risk even though the wrapper is
			// peeled before rule matching (sudo ls -> ls).
			if base0 := filepath.Base(strings.TrimPrefix(ev.words[0], "\\")); base0 == "sudo" || base0 == "doas" {
				risk = true
				return false
			}
			rest, _ := peelWrappersEv(ev)
			if len(rest.words) == 0 || rest.dynami[0] {
				risk = true // unevaluable command name: fail closed
				return false
			}
			if isRiskCall(rest.words) {
				risk = true
				return false
			}
			if body, ok := wrappedBody(rest.words); ok && IsRiskCommand(body) {
				risk = true
				return false
			}
		}
		return true
	})
	return risk
}

// isProtectedTarget: redirects into the guard's protected files.
func isProtectedTarget(p string) bool {
	if p == "" {
		return false
	}
	return isLocksFile(p) || strings.HasSuffix(p, "/locks.yaml") || filepath.Base(p) == "config.yaml"
}

// isRiskCall: one simple command against the narrow risk table.
func isRiskCall(words []string) bool {
	base := filepath.Base(strings.TrimPrefix(words[0], "\\"))
	switch base {
	case "rm", "dd", "shred", "truncate", "chmod", "chown", "chattr", "setfacl",
		"sudo", "doas", "shutdown", "reboot", "poweroff", "halt",
		"mount", "umount", "fdisk", "parted", "kill", "pkill", "killall",
		"mv", "ln", "tee":
		return true
	}
	if base == "mkfs" || strings.HasPrefix(base, "mkfs.") {
		return true
	}
	switch base {
	case "find":
		for _, a := range words[1:] {
			if a == "-delete" || a == "-exec" || strings.HasPrefix(a, "-exec") {
				return true
			}
		}
	case "git":
		if len(words) > 1 {
			switch words[1] {
			case "push":
				for _, a := range words[2:] {
					if a == "-f" || a == "--force" || strings.HasPrefix(a, "--force") {
						return true
					}
				}
			case "clean":
				for _, a := range words[2:] {
					if !strings.HasPrefix(a, "-") {
						continue
					}
					if strings.ContainsAny(a[1:], "fd") {
						return true
					}
				}
			case "reset":
				for _, a := range words[2:] {
					if a == "--hard" {
						return true
					}
				}
			}
		}
	}
	return false
}

// CommandSignature canonicalizes a command into its exact argv
// signature: one entry per simple command (statically evaluated argv,
// NUL-separated), calls joined with "\x01". Dynamic or unparseable
// input returns the raw text unchanged — dynamic commands are never
// allowlisted.
func CommandSignature(cmd string) string {
	prog, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return cmd
	}
	var calls []string
	dynamic := false
	syntax.Walk(prog, func(node syntax.Node) bool {
		if c, ok := node.(*syntax.CallExpr); ok {
			ev := evalWords(c.Args)
			if ev.dynamic() {
				dynamic = true
				return false
			}
			calls = append(calls, strings.Join(ev.words, "\x00"))
		}
		return true
	})
	if dynamic || len(calls) == 0 {
		return cmd
	}
	return strings.Join(calls, "\x01")
}
