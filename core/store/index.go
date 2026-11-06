package store

// Git index (v2) writer. After each auto-commit the index lists every
// tracked file with its blob sha, so real `git status` reports a clean
// tree.

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

// writeIndex rebuilds .git/index from the current working tree state
// that the last commit captured (treeID of HEAD).
func (s *Store) writeIndex() error {
	head, err := s.Head()
	if err != nil || head == "" {
		return err
	}
	treeID, err := s.commitTree(head)
	if err != nil {
		return err
	}
	flat, err := s.flattenTree(treeID, "")
	if err != nil {
		return err
	}
	// Sorted entries like git expects.
	paths := make([]string, 0, len(flat))
	for p := range flat {
		paths = append(paths, p)
	}
	sortStrings(paths)

	var buf bytes.Buffer
	buf.WriteString("DIRC")
	binary.Write(&buf, binary.BigEndian, uint32(2)) // version
	binary.Write(&buf, binary.BigEndian, uint32(len(paths)))
	for _, p := range paths {
		blob := flat[p]
		full := filepath.Join(s.Dir, filepath.FromSlash(p))
		var mtime uint32
		var size uint32
		if fi, err := os.Stat(full); err == nil {
			mtime = uint32(fi.ModTime().Unix())
			size = uint32(fi.Size())
		}
		raw, err := decodeHex(blob)
		if err != nil {
			return err
		}
		flags := uint16(len(p))
		if len(p) >= 0xFFF {
			flags = 0xFFF
		}
		var e bytes.Buffer
		binary.Write(&e, binary.BigEndian, uint32(0))        // ctime sec
		binary.Write(&e, binary.BigEndian, uint32(0))        // ctime nsec
		binary.Write(&e, binary.BigEndian, mtime)            // mtime sec
		binary.Write(&e, binary.BigEndian, uint32(0))        // mtime nsec
		binary.Write(&e, binary.BigEndian, uint32(0))        // dev
		binary.Write(&e, binary.BigEndian, uint32(0))        // ino
		binary.Write(&e, binary.BigEndian, uint32(0o100644)) // mode
		binary.Write(&e, binary.BigEndian, uint32(0))        // uid
		binary.Write(&e, binary.BigEndian, uint32(0))        // gid
		binary.Write(&e, binary.BigEndian, size)             // size
		e.Write(raw)
		e.WriteByte(byte(flags >> 8))
		e.WriteByte(byte(flags & 0xFF))
		e.WriteString(p)
		e.WriteByte(0) // NUL
		total := 62 + len(p) + 1
		pad := (8 - total%8) % 8
		e.Write(make([]byte, pad))
		buf.Write(e.Bytes())
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return os.WriteFile(filepath.Join(s.GitDir, "index"), buf.Bytes(), 0o644)
}

func decodeHex(s string) ([]byte, error) {
	if len(s) != 40 {
		return nil, fmt.Errorf("bad sha %q", s)
	}
	out := make([]byte, 20)
	for i := 0; i < 20; i++ {
		v := 0
		for _, c := range s[i*2 : i*2+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= int(c - '0')
			case c >= 'a' && c <= 'f':
				v |= int(c-'a') + 10
			default:
				return nil, fmt.Errorf("bad hex %q", s)
			}
		}
		out[i] = byte(v)
	}
	return out, nil
}

func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}
