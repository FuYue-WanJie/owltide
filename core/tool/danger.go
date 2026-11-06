package tool

// Dangerous-command detection and the user-confirmation gate. Tools
// that spot a dangerous action call confirmDanger: with no confirmer
// wired (tests, one-shot runs) the action is rejected; with a
// confirmer the user decides. The model cannot approve for the user.

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// Confirmer asks the user a yes/no question.
type Confirmer func(prompt string) bool

// confirmerHolder is implemented by tools that can ask the user.
type confirmerHolder interface {
	setConfirmer(Confirmer)
}

// dangerPatterns match shell commands that need explicit user sign-off.
var dangerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+(-\w+\s+)*-\w*[rR]\w*\s+(-\w+\s+)*-\w*[fF]|(-\w+\s+)*-\w*[fF]\w*\s+(-\w+\s+)*-\w*[rR]`),
	regexp.MustCompile(`\brm\s+(-[a-zA-Z]*[rR][a-zA-Z]*[fF]|-[a-zA-Z]*[fF][a-zA-Z]*[rR])\b`),
	regexp.MustCompile(`\brm\s+(-r|-R|-rf|-fr|-Rr|-rR)\s+.*\s+-[fF]\b|\brm\s+-[fF]\w*\s+\S+\s+-[rR]`),
	regexp.MustCompile(`\bgit\s+push\b[^\n]*(-f\b|--force\b|--force-with-lease\b)`),
	regexp.MustCompile(`\bgit\s+reset\s+--hard\b`),
	regexp.MustCompile(`\bmkfs(\.\w+)?\b`),
	regexp.MustCompile(`\bdd\s+.*of=/dev/`),
	regexp.MustCompile(`\bchmod\s+-R\s+777\b`),
	regexp.MustCompile(`\bshutdown\b|\breboot\b|\bpoweroff\b`),
	regexp.MustCompile(`>\s*/dev/sd[a-z]`),
}

// isDangerousCommand reports whether a shell command needs confirmation.
func isDangerousCommand(cmd string) bool {
	for _, re := range dangerPatterns {
		if re.MatchString(cmd) {
			return true
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
