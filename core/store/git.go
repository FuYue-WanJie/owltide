// Package store implements a minimal git-compatible object store
// used for automatic commits of ~/.owltide. It speaks the standard
// git formats (loose objects, tree/commit, refs) with only the
// standard library, so it works on Android where the git binary
// may not exist. Real `git` can read the resulting history.
package store

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store is a git repository rooted at dir (the working tree).
type Store struct {
	Dir    string
	GitDir string // Dir/.git
	Now    func() time.Time
	Who    string // default author, e.g. "owltide"
}

// Open initializes dir/.git if needed and returns a Store.
func Open(dir string) (*Store, error) {
	s := &Store{Dir: dir, GitDir: filepath.Join(dir, ".git"),
		Now: time.Now, Who: "owltide"}
	for _, sub := range []string{"objects", "refs/heads"} {
		if err := os.MkdirAll(filepath.Join(s.GitDir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(s.headPath()); os.IsNotExist(err) {
		if err := os.WriteFile(s.headPath(), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
			return nil, err
		}
	}
	// Repair a missing index after external git operations (e.g. gc).
	_ = s.writeIndex()
	return s, nil
}

func (s *Store) headPath() string { return filepath.Join(s.GitDir, "HEAD") }

// Head returns the current commit id, or "" when unborn.
func (s *Store) Head() (string, error) {
	b, err := os.ReadFile(s.headPath())
	if err != nil {
		return "", err
	}
	ref := strings.TrimSpace(strings.TrimPrefix(string(b), "ref: "))
	id, err := os.ReadFile(filepath.Join(s.GitDir, filepath.FromSlash(ref)))
	if os.IsNotExist(err) {
		// git gc packs loose refs into packed-refs.
		if pid, perr := s.readPackedRef(ref); perr == nil {
			return pid, nil
		}
		return "", nil
	}
	return strings.TrimSpace(string(id)), err
}

// readPackedRef resolves a ref from .git/packed-refs.
func (s *Store) readPackedRef(ref string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.GitDir, "packed-refs"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue
		}
		if i := strings.IndexByte(line, ' '); i > 0 && line[i+1:] == ref {
			return line[:i], nil
		}
	}
	return "", os.ErrNotExist
}

// writeObject stores payload under its git type and returns the id.
func (s *Store) writeObject(typ string, payload []byte) (string, error) {
	full := append([]byte(typ+" "+fmt.Sprint(len(payload))+"\x00"), payload...)
	sum := sha1.Sum(full)
	id := hex.EncodeToString(sum[:])
	path := filepath.Join(s.GitDir, "objects", id[:2])
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(path, id[2:])
	if _, err := os.Stat(dst); err == nil {
		return id, nil // dedup
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(full); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return id, os.WriteFile(dst, buf.Bytes(), 0o444)
}

// ReadObjectExport exposes object reading to sibling packages.
func (s *Store) ReadObjectExport(id string) (string, []byte, error) { return s.readObject(id) }

// readObject loads an object: loose first, then pack files.
func (s *Store) readObject(id string) (typ string, payload []byte, err error) {
	typ, payload, lerr := s.readLoose(id)
	if lerr == nil {
		return typ, payload, nil
	}
	return s.readFromPacks(id)
}

// readLoose decodes a loose object only.
func (s *Store) readLoose(id string) (typ string, payload []byte, err error) {
	raw, err := os.ReadFile(filepath.Join(s.GitDir, "objects", id[:2], id[2:]))
	if err != nil {
		return "", nil, err
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", nil, err
	}
	defer zr.Close()
	var full bytes.Buffer
	if _, err := full.ReadFrom(zr); err != nil {
		return "", nil, err
	}
	nul := bytes.IndexByte(full.Bytes(), 0)
	if nul < 0 {
		return "", nil, fmt.Errorf("corrupt object %s", id)
	}
	header := string(full.Bytes()[:nul])
	sp := strings.IndexByte(header, ' ')
	return header[:sp], full.Bytes()[nul+1:], nil
}

type treeEntry struct {
	mode string
	name string
	id   string
}

// writeTree hashes one directory (non-recursive; config dirs are flat).
func (s *Store) writeTree(rel string) (string, []treeEntry, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, rel))
	if err != nil {
		return "", nil, err
	}
	var out []treeEntry
	for _, e := range entries {
		if e.Name() == ".git" || (rel == "." && (e.Name() == stashDir || e.Name() == "logs" || e.Name() == "spill" || e.Name() == "sessions")) {
			continue
		}
		rp := filepath.ToSlash(filepath.Join(rel, e.Name()))
		if e.IsDir() {
			id, _, err := s.writeTree(rp)
			if err != nil {
				return "", nil, err
			}
			out = append(out, treeEntry{"40000", e.Name(), id})
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, rp))
		if err != nil {
			return "", nil, err
		}
		id, err := s.writeObject("blob", data)
		if err != nil {
			return "", nil, err
		}
		out = append(out, treeEntry{"100644", e.Name(), id})
	}
	sort.Slice(out, func(i, j int) bool {
		return entryKey(out[i]) < entryKey(out[j])
	})
	var buf bytes.Buffer
	for _, en := range out {
		fmt.Fprintf(&buf, "%s %s\x00", en.mode, en.name)
		raw, _ := hex.DecodeString(en.id)
		buf.Write(raw)
	}
	id, err := s.writeObject("tree", buf.Bytes())
	return id, out, err
}

// entryKey sorts like git: directory names get a trailing slash.
func entryKey(e treeEntry) string {
	if e.mode == "40000" {
		return e.name + "/"
	}
	return e.name
}

// snapshot commits the whole working tree; returns the new commit id
// and the changed paths since the previous commit (empty map on first).
func (s *Store) snapshot(msg string) (string, map[string]bool, error) {
	treeID, _, err := s.writeTree(".")
	if err != nil {
		return "", nil, err
	}
	parent, err := s.Head()
	if err != nil {
		return "", nil, err
	}
	if parent != "" {
		if pTree, err := s.commitTree(parent); err == nil && pTree == treeID {
			return parent, nil, nil // nothing changed
		}
	}
	id, changed, err := s.commit(treeID, parent, msg)
	return id, changed, err
}

func (s *Store) commitTree(commitID string) (string, error) {
	_, payload, err := s.readObject(commitID)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(payload), "\n") {
		if strings.HasPrefix(line, "tree ") {
			return strings.Fields(line)[1], nil
		}
	}
	return "", fmt.Errorf("commit %s has no tree", commitID)
}

func (s *Store) commit(treeID, parent, msg string) (string, map[string]bool, error) {
	changed := map[string]bool{}
	if parent != "" {
		if oldTree, err := s.commitTree(parent); err == nil {
			changed, err = s.diffTrees(oldTree, treeID)
			if err != nil {
				return "", nil, err
			}
		}
	}
	var buf bytes.Buffer
	ts := s.Now().UTC()
	fmt.Fprintf(&buf, "tree %s\n", treeID)
	if parent != "" {
		fmt.Fprintf(&buf, "parent %s\n", parent)
	}
	ident := s.Who + " <owltide@local> " + fmt.Sprint(ts.Unix()) + " +0000"
	fmt.Fprintf(&buf, "author %s\ncommitter %s\n\n%s\n", ident, ident, msg)
	id, err := s.writeObject("commit", buf.Bytes())
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(
		filepath.Join(s.GitDir, "refs", "heads", "main"), []byte(id+"\n"), 0o644); err != nil {
		return "", nil, err
	}
	// A loose ref shadows packed-refs, but git gc may have packed it:
	// strip the stale packed entry to avoid ambiguity.
	s.removePackedRef("refs/heads/main")
	// Keep .git/index in sync so real git status stays clean.
	_ = s.writeIndex()
	return id, changed, nil
}

// removePackedRef deletes one ref line from packed-refs (best effort).
func (s *Store) removePackedRef(ref string) {
	data, err := os.ReadFile(filepath.Join(s.GitDir, "packed-refs"))
	if err != nil {
		return
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || line[0] == '#' || line[0] == '^' {
			out = append(out, line)
			continue
		}
		if i := strings.IndexByte(line, ' '); i > 0 && line[i+1:] == ref {
			continue // drop
		}
		out = append(out, line)
	}
	_ = os.WriteFile(filepath.Join(s.GitDir, "packed-refs"),
		[]byte(strings.Join(out, "\n")), 0o644)
}

// diffTrees compares two trees (recursive, flat-entries flattened to
// file paths) and returns changed file paths.
func (s *Store) diffTrees(a, b string) (map[string]bool, error) {
	fa, err := s.flattenTree(a, "")
	if err != nil {
		return nil, err
	}
	fb, err := s.flattenTree(b, "")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for name, id := range fa {
		if fb[name] != id {
			out[name] = true
		}
	}
	for name, id := range fb {
		if fa[name] != id {
			out[name] = true
		}
	}
	return out, nil
}

// flattenTree recursively maps file path -> blob id.
func (s *Store) flattenTree(id, prefix string) (map[string]string, error) {
	typ, payload, err := s.readObject(id)
	if err != nil {
		return nil, err
	}
	if typ != "tree" {
		return nil, fmt.Errorf("%s is a %s, want tree", id, typ)
	}
	out := map[string]string{}
	raw := payload
	for len(raw) > 0 {
		sp := bytes.IndexByte(raw, ' ')
		nul := bytes.IndexByte(raw, 0)
		if sp < 0 || nul < 0 || nul+21 > len(raw) {
			return nil, fmt.Errorf("corrupt tree %s", id)
		}
		name := string(raw[sp+1 : nul])
		hexID := hex.EncodeToString(raw[nul+1 : nul+21])
		raw = raw[nul+21:]
		if strings.HasPrefix(name, prefix) == false && prefix != "" {
			// nested entries carry their own name; nothing to do
		}
		if len(hexID) == 40 {
			// probe type: cheap via readObject
			ot, _, err := s.readObject(hexID)
			if err != nil {
				return nil, err
			}
			if ot == "tree" {
				sub, err := s.flattenTree(hexID, prefix+name+"/")
				if err != nil {
					return nil, err
				}
				for k, v := range sub {
					out[k] = v
				}
				continue
			}
		}
		out[prefix+name] = hexID
	}
	return out, nil
}

func (s *Store) readTree(id string) (map[string]string, error) {
	typ, payload, err := s.readObject(id)
	if err != nil {
		return nil, err
	}
	if typ != "tree" {
		return nil, fmt.Errorf("%s is a %s, want tree", id, typ)
	}
	out := map[string]string{}
	raw := payload
	for len(raw) > 0 {
		sp := bytes.IndexByte(raw, ' ')
		nul := bytes.IndexByte(raw, 0)
		if sp < 0 || nul < 0 || nul+21 > len(raw) {
			return nil, fmt.Errorf("corrupt tree %s", id)
		}
		name := string(raw[sp+1 : nul])
		hexID := hex.EncodeToString(raw[nul+1 : nul+21])
		out[name] = hexID
		raw = raw[nul+21:]
	}
	return out, nil
}
