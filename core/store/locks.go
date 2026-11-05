package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Locks is the protected-set registry: ~/.owltide/locks.yaml.
// The file itself and config:budget are permanently locked and can
// never be removed from the list by the agent.
type Locks struct {
	Items []string `yaml:"locked"`
}

// Permanent returns lock ids that can never be unlocked.
func Permanent() []string { return []string{"locks", "config:budget"} }

// locksPath is the on-disk location.
func (s *Store) locksPath() string { return filepath.Join(s.Dir, "locks.yaml") }

// LoadLocks reads locks.yaml; missing file means empty list.
func (s *Store) LoadLocks() (Locks, error) {
	var l Locks
	data, err := os.ReadFile(s.locksPath())
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	err = yaml.Unmarshal(data, &l)
	return l, err
}

// IsLocked reports whether id is protected (including permanent ids).
func (s *Store) IsLocked(id string) bool {
	for _, p := range Permanent() {
		if id == p {
			return true
		}
	}
	l, err := s.LoadLocks()
	if err != nil {
		return true // fail closed
	}
	for _, it := range l.Items {
		if it == id {
			return true
		}
	}
	return false
}

// Lock adds id to the list (user-only CLI path); fails if permanent.
func (s *Store) Lock(id string) error {
	for _, p := range Permanent() {
		if id == p {
			return fmt.Errorf("%s is permanently locked", id)
		}
	}
	l, err := s.LoadLocks()
	if err != nil {
		return err
	}
	for _, it := range l.Items {
		if it == id {
			return fmt.Errorf("%s already locked", id)
		}
	}
	l.Items = append(l.Items, id)
	sort.Strings(l.Items)
	return s.saveLocks(l, CommitMeta{Who: "user", Reason: "lock " + id})
}

// Unlock removes id from the list; permanent ids refuse.
func (s *Store) Unlock(id string) error {
	for _, p := range Permanent() {
		if id == p {
			return fmt.Errorf("%s is permanently locked", id)
		}
	}
	l, err := s.LoadLocks()
	if err != nil {
		return err
	}
	out := l.Items[:0]
	found := false
	for _, it := range l.Items {
		if it == id {
			found = true
			continue
		}
		out = append(out, it)
	}
	if !found {
		return fmt.Errorf("%s is not locked", id)
	}
	l.Items = out
	return s.saveLocks(l, CommitMeta{Who: "user", Reason: "unlock " + id})
}

// saveLocks writes the file and records it in history. The file itself
// is always locked for agent writes; here we are the user path.
func (s *Store) saveLocks(l Locks, meta CommitMeta) error {
	data, err := yaml.Marshal(l)
	if err != nil {
		return err
	}
	old, _ := s.Head()
	if err := os.WriteFile(s.locksPath(), data, 0o600); err != nil {
		return err
	}
	_, _, err = s.AutoCommit(meta)
	if err != nil {
		// best effort restore
		if old != "" {
			if c, e := s.loadCommit(old); e == nil {
				_ = s.Checkout(c.ID)
			}
		}
	}
	return err
}
