// Package guard enforces protected-file integrity at the transaction
// level: snapshots hashes of protected files before each tool call and
// restores them if a tool (including run_shell) mutated them. This is
// a backstop behind the tool-entry gates, not a sandbox.
package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FuYue-WanJie/owltide/core/store"
)

// Guard tracks protected files under the config root.
type Guard struct {
	Root string // ~/.owltide
	St   *store.Store
	// ExtraPaths are additional absolute paths to protect (set by tests).
	ExtraPaths []string
}

// protectedPaths lists every file the agent must never change, by
// relative path within Root. Permanent locks + the lock registry.
func (g *Guard) protectedPaths() []string {
	out := []string{"locks.yaml", "config.yaml"}
	for _, id := range store.Permanent() { // "locks", "config:budget"
		out = append(out, id)
	}
	return out
}

// paths resolves the protected set to absolute paths. It re-reads the
// lock list every snapshot so newly locked items are covered.
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
	locks, err := g.St.LoadLocks()
	if err == nil {
		for _, id := range locks.Items {
			for _, p := range pathsForLockID(g.Root, id) {
				add(p)
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

// Snapshot is a set of protected-file hashes (abs path -> sha256).
type Snapshot map[string]string

func hashFile(p string) (string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // absent is a state too
		}
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Snapshot hashes all protected files.
func (g *Guard) Snapshot() (Snapshot, error) {
	snap := Snapshot{}
	for _, p := range g.paths() {
		h, err := hashFile(p)
		if err != nil {
			return nil, err
		}
		snap[p] = h
	}
	return snap, nil
}

// Verify compares the current state against a snapshot; changed files
// are restored from the store (best effort) and returned.
func (g *Guard) Verify(before Snapshot) ([]string, error) {
	var violated []string
	for _, p := range g.paths() {
		now, err := hashFile(p)
		if err != nil {
			return violated, err
		}
		if before[p] != now {
			violated = append(violated, p)
			g.restore(p)
		}
	}
	return violated, nil
}

// restore puts a protected file back from HEAD's tree (best effort).
func (g *Guard) restore(abs string) {
	head, err := g.St.Head()
	if err != nil || head == "" {
		return
	}
	treeID, err := g.St.CommitTreeExport(head)
	if err != nil {
		return
	}
	flat, err := g.St.FlatTreeExport(treeID, "")
	if err != nil {
		return
	}
	rel, err := filepath.Rel(g.Root, abs)
	if err != nil {
		return
	}
	blob, ok := flat[rel]
	if !ok {
		return // file is not tracked; leave as-is
	}
	typ, data, err := g.St.ReadObjectExport(blob)
	if err != nil || typ != "blob" {
		return
	}
	os.MkdirAll(filepath.Dir(abs), 0o755)
	os.WriteFile(abs, data, 0o600)
}

var _ = fmt.Sprintf
