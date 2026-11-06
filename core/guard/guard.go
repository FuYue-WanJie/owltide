// Package guard enforces protected-file integrity at the transaction
// level. Every tool call is bracketed by a full snapshot (content +
// permissions + absence) of the protected set; any mutation — by any
// tool including run_shell — is restored exactly and reported. Any
// snapshot/verify error is treated as a violation, never skipped:
// the tool is refused, files are restored from the last trusted
// snapshot, and a failed restore aborts the run.
//
// Delayed writes (nohup ... & ) are caught by re-verifying against
// the last trusted snapshot before every model call. This is still
// not a sandbox: an adversary with arbitrary code execution can race
// the verifier; the confirmer and the user remain the final authority.
package guard

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FuYue-WanJie/owltide/core/store"
)

// Entry is one protected file's state at snapshot time.
type Entry struct {
	Absent bool
	Mode   os.FileMode // perm bits for regular files
	Data   []byte      // content (protected files are small)
}

// Snapshot is the trusted state of the protected set.
type Snapshot map[string]Entry

// Guard tracks protected files under the config root.
type Guard struct {
	Root string // ~/.owltide
	St   *store.Store
	// ExtraPaths are additional absolute paths to protect (set by tests).
	ExtraPaths []string
}

// paths resolves the protected set to absolute paths. It re-reads the
// lock list so newly locked items are covered.
func (g *Guard) paths() []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, rel := range []string{"locks.yaml", "config.yaml"} {
		add(filepath.Join(g.Root, rel))
	}
	if g.St != nil {
		if locks, err := g.St.LoadLocks(); err == nil {
			for _, id := range locks.Items {
				for _, p := range pathsForLockID(g.Root, id) {
					add(p)
				}
			}
		}
	}
	for _, p := range g.ExtraPaths {
		add(p)
	}
	sort.Strings(out)
	return out
}

// pathsForLockID maps a lock id to concrete files (mirrors evolve).
func pathsForLockID(root, id string) []string {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return nil
	}
	switch parts[0] {
	case "config":
		return []string{filepath.Join(root, "config.yaml")}
	case "locks":
		return []string{filepath.Join(root, "locks.yaml")}
	case "agent":
		return []string{filepath.Join(root, "agents", parts[1]+".md")}
	case "skill":
		return []string{filepath.Join(root, "skills", parts[1]+".md")}
	case "memory":
		return []string{filepath.Join(root, "memory", parts[1]+".md")}
	}
	return nil
}

// Snapshot captures content, permissions, and absence of every
// protected path. Symlinks in the protected set are an error: they
// redirect writes outside the managed tree.
func (g *Guard) Snapshot() (Snapshot, error) {
	snap := Snapshot{}
	for _, p := range g.paths() {
		e, err := readEntry(p)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", p, err)
		}
		snap[p] = e
	}
	return snap, nil
}

func readEntry(p string) (Entry, error) {
	fi, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return Entry{Absent: true}, nil
	}
	if err != nil {
		return Entry{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return Entry{}, fmt.Errorf("symlink in protected set")
	}
	if !fi.Mode().IsRegular() {
		return Entry{}, fmt.Errorf("non-regular file in protected set")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Mode: fi.Mode().Perm(), Data: data}, nil
}

// Verify compares the working tree against the trusted snapshot using
// the UNION of snapshotted paths and the current protected set (a
// tampered locks.yaml must not shrink the set mid-check). Violations
// are restored to the snapshot state exactly — including deleting
// files that were absent before and restoring permissions. Restore
// failures are returned, never swallowed.
func (g *Guard) Verify(before Snapshot) (violated, failed []string) {
	// Union: everything snapshotted plus everything protected now.
	set := map[string]bool{}
	for p := range before {
		set[p] = true
	}
	for _, p := range g.paths() {
		set[p] = true
	}
	ordered := make([]string, 0, len(set))
	for p := range set {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)

	for _, p := range ordered {
		was, known := before[p]
		if !known {
			// Protected now but absent from the snapshot: creation of
			// a new protected file mid-run is a violation.
			if _, err := os.Lstat(p); err == nil {
				violated = append(violated, p)
				if err := os.Remove(p); err != nil {
					failed = append(failed, p+": remove: "+err.Error())
				}
			}
			continue
		}
		cur, err := readEntry(p)
		if err != nil {
			// Unreadable (chmod 000), symlinked, etc: a violation.
			violated = append(violated, p)
			if rerr := g.restore(p, was); rerr != nil {
				failed = append(failed, p+": "+rerr.Error())
			}
			continue
		}
		if !sameEntry(cur, was) {
			violated = append(violated, p)
			if rerr := g.restore(p, was); rerr != nil {
				failed = append(failed, p+": "+rerr.Error())
			}
		}
	}
	return violated, failed
}

func sameEntry(a, b Entry) bool {
	if a.Absent != b.Absent {
		return false
	}
	if a.Absent {
		return true
	}
	if a.Mode != b.Mode {
		return false
	}
	sumA := sha256.Sum256(a.Data)
	sumB := sha256.Sum256(b.Data)
	return sumA == sumB
}

// restore puts one file back exactly as snapshotted. A chmod first so
// a chmod 000 tamper cannot block the rewrite; a path swapped to a
// directory or symlink is removed wholesale.
func (g *Guard) restore(p string, e Entry) error {
	if e.Absent {
		return os.RemoveAll(p)
	}
	if fi, err := os.Lstat(p); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || fi.IsDir() {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
		} else if err := os.Chmod(p, 0o600); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, e.Data, e.Mode); err != nil {
		return err
	}
	return os.Chmod(p, e.Mode)
}
