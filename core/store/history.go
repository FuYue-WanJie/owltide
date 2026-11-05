package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Commit is one entry of the history log.
type Commit struct {
	ID      string
	Parent  string
	Msg     string
	When    string // RFC3339, parsed from ident
	Author  string
	Changed []string // file paths changed vs parent (best effort)
}

// Log walks history from HEAD, newest first.
func (s *Store) Log() ([]Commit, error) {
	var out []Commit
	id, err := s.Head()
	if err != nil || id == "" {
		return out, err
	}
	for id != "" {
		c, err := s.loadCommit(id)
		if err != nil {
			return out, err
		}
		out = append(out, c)
		id = c.Parent
	}
	return out, nil
}

// ExportLoadCommit exposes commit loading to sibling packages.
func (s *Store) ExportLoadCommit(id string) (Commit, error) { return s.loadCommit(id) }

func (s *Store) loadCommit(id string) (Commit, error) {
	typ, payload, err := s.readObject(id)
	if err != nil {
		return Commit{}, err
	}
	if typ != "commit" {
		return Commit{}, fmt.Errorf("%s is a %s", id, typ)
	}
	c := Commit{ID: id}
	head, body, _ := strings.Cut(string(payload), "\n\n")
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "parent "):
			c.Parent = strings.Fields(line)[1]
		case strings.HasPrefix(line, "author "):
			c.Author, c.When = parseIdent(line[7:])
		}
	}
	c.Msg = strings.TrimSpace(body)
	if c.Parent != "" {
		aTree, errA := s.commitTree(c.Parent)
		bTree, errB := s.commitTree(c.ID)
		if errA == nil && errB == nil {
			if diff, err := s.diffTrees(aTree, bTree); err == nil {
				for p := range diff {
					c.Changed = append(c.Changed, p)
				}
			}
		}
	}
	return c, nil
}

// parseIdent reads "Name <mail> ts tz".
func parseIdent(s string) (name, when string) {
	lt := strings.IndexByte(s, '<')
	if lt > 0 {
		name = strings.TrimSpace(s[:lt])
	}
	fields := strings.Fields(s)
	for i, f := range fields {
		if len(f) > 0 && f[0] >= '1' && f[0] <= '9' {
			_ = i
			if ts := f; len(ts) == 10 {
				when = toRFC3339(ts)
				break
			}
		}
	}
	return name, when
}

// Checkout restores the working tree to a commit (flat dirs only).
func (s *Store) Checkout(commitID string) error {
	treeID, err := s.commitTree(commitID)
	if err != nil {
		return err
	}
	// Remove tracked files/dirs not present in the target tree.
	flat, err := s.flattenTree(treeID, "")
	if err != nil {
		return err
	}
	keep := map[string]bool{} // top-level names present in target
	for name := range flat {
		keep[strings.SplitN(name, "/", 2)[0]] = true
	}
	entries, _ := os.ReadDir(s.Dir)
	for _, e := range entries {
		if e.Name() == ".git" || e.Name() == "logs" {
			continue
		}
		if !keep[e.Name()] {
			if err := os.RemoveAll(filepath.Join(s.Dir, e.Name())); err != nil {
				return err
			}
		}
	}
	if err := s.materialize(treeID, ""); err != nil {
		return err
	}
	return nil
}

// materialize recursively writes tree objects to the working tree.
func (s *Store) materialize(treeID, prefix string) error {
	tree, err := s.readTree(treeID)
	if err != nil {
		return err
	}
	// Build a sorted deterministic order.
	names := make([]string, 0, len(tree))
	for n := range tree {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		id := tree[name]
		typ, payload, err := s.readObject(id)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		switch typ {
		case "blob":
			dst := filepath.Join(s.Dir, filepath.FromSlash(prefix+name))
			os.MkdirAll(filepath.Dir(dst), 0o755)
			if err := os.WriteFile(dst, payload, 0o644); err != nil {
				return err
			}
		case "tree":
			if err := s.materialize(id, prefix+name+"/"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected object type %s for %s", typ, name)
		}
	}
	return nil
}

// RevertLast redoes the previous state: checkout parent then commit
// the revert so history keeps moving forward.
func (s *Store) RevertLast(reason string) (string, error) {
	head, err := s.Head()
	if err != nil {
		return "", err
	}
	if head == "" {
		return "", fmt.Errorf("nothing to revert")
	}
	c, err := s.loadCommit(head)
	if err != nil {
		return "", err
	}
	if c.Parent == "" {
		return "", fmt.Errorf("cannot revert the first commit")
	}
	if err := s.Checkout(c.Parent); err != nil {
		return "", err
	}
	id, _, err := s.snapshot("revert: " + reason)
	return id, err
}

// ResolveCommit expands a short commit id to the full one.
func (s *Store) ResolveCommit(id string) (string, error) {
	log, err := s.Log()
	if err != nil {
		return "", err
	}
	for _, c := range log {
		if strings.HasPrefix(c.ID, id) {
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("commit %q not found", id)
}
