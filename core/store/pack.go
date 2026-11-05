package store

// Pack file reading (git pack v2 + idx v2). Only what the commit
// engine needs: locate an object by SHA-1 in packs and inflate it,
// resolving ofs/ref deltas. Writing stays loose-only; real git can
// pack our loose objects and we keep reading them.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// packEntry is one object found in a pack.
type packEntry struct {
	packPath string
	offset   uint64
}

// packIndex maps sha -> packEntry, built lazily per idx file.
type packIndex struct {
	idxPath  string
	packPath string
	// sorted sha1s and their offsets from idx v2
	shas     [][]byte
	offsets  []uint64
	parsedOK bool
}

// readObjectLooseOrPack resolves an object from loose storage or any
// pack in objects/pack.
func (s *Store) readObjectAny(id string) (typ string, payload []byte, err error) {
	// Loose first (fast path, our own writes).
	if typ, payload, err := s.readObject(id); err == nil {
		return typ, payload, nil
	}
	return s.readFromPacks(id)
}

func (s *Store) packDir() string { return filepath.Join(s.GitDir, "objects", "pack") }

// readFromPacks searches every pack index for the id.
func (s *Store) readFromPacks(id string) (string, []byte, error) {
	bin, _ := hex.DecodeString(id)
	packs, err := filepath.Glob(filepath.Join(s.packDir(), "*.idx"))
	if err != nil {
		return "", nil, err
	}
	sort.Strings(packs)
	var lastErr error
	for _, idx := range packs {
		pi, err := loadPackIndex(idx)
		if err != nil {
			lastErr = err
			continue
		}
		off, ok := pi.lookup(bin)
		if !ok {
			continue
		}
		return s.readPackObject(pi.packPath, off, 0)
	}
	if lastErr != nil {
		return "", nil, lastErr
	}
	return "", nil, os.ErrNotExist
}

// loadPackIndex parses a git idx v2 file.
func loadPackIndex(idxPath string) (*packIndex, error) {
	pi := &packIndex{
		idxPath:  idxPath,
		packPath: idxPath[:len(idxPath)-len(".idx")] + ".pack",
	}
	data, err := os.ReadFile(idxPath)
	if err != nil {
		return nil, err
	}
	// Magic: \377tOc, version 2.
	if len(data) < 8 || !bytes.Equal(data[:4], []byte{0xff, 't', 'O', 'c'}) {
		return nil, fmt.Errorf("unsupported idx %s", idxPath)
	}
	ver := binary.BigEndian.Uint32(data[4:8])
	if ver != 2 {
		return nil, fmt.Errorf("idx v%d unsupported", ver)
	}
	// 256 fanout entries then N sha20 then N crc4 then N off4.
	n := int(binary.BigEndian.Uint32(data[8+255*4 : 8+256*4]))
	base := 8 + 256*4
	shas := make([][]byte, n)
	for i := 0; i < n; i++ {
		shas[i] = data[base+i*20 : base+i*20+20]
	}
	offBase := base + n*20 + n*4
	offsets := make([]uint64, n)
	for i := 0; i < n; i++ {
		v := binary.BigEndian.Uint32(data[offBase+i*4 : offBase+i*4+4])
		if v&0x80000000 != 0 {
			// 64-bit offset table follows; index = v & 0x7fffffff
			largeBase := offBase + n*4
			idx64 := int(v & 0x7fffffff)
			offsets[i] = binary.BigEndian.Uint64(data[largeBase+idx64*8 : largeBase+idx64*8+8])
		} else {
			offsets[i] = uint64(v)
		}
	}
	pi.shas = shas
	pi.offsets = offsets
	pi.parsedOK = true
	return pi, nil
}

// lookup finds the offset by sha (binary search over sorted fanout order).
func (pi *packIndex) lookup(sha []byte) (uint64, bool) {
	lo, hi := 0, len(pi.shas)
	for lo < hi {
		mid := (lo + hi) / 2
		switch bytes.Compare(pi.shas[mid], sha) {
		case 0:
			return pi.offsets[mid], true
		case -1:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return 0, false
}

// readPackObject inflates one object at offset; depth guards delta chains.
func (s *Store) readPackObject(packPath string, off uint64, depth int) (string, []byte, error) {
	if depth > 50 {
		return "", nil, fmt.Errorf("delta chain too deep")
	}
	f, err := os.Open(packPath)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	if _, err := f.Seek(int64(off), io.SeekStart); err != nil {
		return "", nil, err
	}
	// Object header: variable-length; (byte>>4)&7 type; size 7-bit groups.
	var hdr []byte
	b := []byte{0}
	for {
		if _, err := io.ReadFull(f, b); err != nil {
			return "", nil, err
		}
		hdr = append(hdr, b[0])
		if b[0]&0x80 == 0 {
			break
		}
	}
	typCode := int((hdr[0] >> 4) & 7)
	// size not needed for full inflate.

	switch typCode {
	case 1, 2, 3, 4: // commit, tree, blob, tag
		typ := map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}[typCode]
		payload, err := inflateStream(f)
		return typ, payload, err
	case 6: // OBJ_OFS_DELTA
		// negative offset varint
		var ofs uint64
		var shift uint
		for {
			if _, err := io.ReadFull(f, b); err != nil {
				return "", nil, err
			}
			ofs = (ofs + 1) << shift
			ofs += uint64(b[0] & 0x7f)
			shift += 7
			if b[0]&0x80 == 0 {
				break
			}
		}
		baseOff := off - ofs
		baseType, basePayload, err := s.readPackObject(packPath, baseOff, depth+1)
		if err != nil {
			return "", nil, err
		}
		delta, err := inflateStream(f)
		if err != nil {
			return "", nil, err
		}
		out, err := applyDelta(basePayload, delta)
		if err != nil {
			return "", nil, err
		}
		return baseType, out, nil
	case 7: // OBJ_REF_DELTA
		ref := make([]byte, 20)
		if _, err := io.ReadFull(f, ref); err != nil {
			return "", nil, err
		}
		baseType, basePayload, err := s.readObjectAny(hex.EncodeToString(ref))
		if err != nil {
			return "", nil, err
		}
		delta, err := inflateStream(f)
		if err != nil {
			return "", nil, err
		}
		out, err := applyDelta(basePayload, delta)
		if err != nil {
			return "", nil, err
		}
		return baseType, out, nil
	}
	return "", nil, fmt.Errorf("unsupported pack object type %d", typCode)
}

// inflateStream reads a zlib stream from the current position.
func inflateStream(r io.Reader) ([]byte, error) {
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out bytes.Buffer
	if _, err := io.Copy(&out, zr); err != nil && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return out.Bytes(), nil
}

// applyDelta applies a git delta buffer to the base payload.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() uint64 {
		var v uint64
		var shift uint
		for pos < len(delta) {
			c := delta[pos]
			pos++
			v |= uint64(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				break
			}
		}
		return v
	}
	readVarint() // base size (ignored)
	targetSize := readVarint()
	out := make([]byte, 0, targetSize)
	for pos < len(delta) {
		c := delta[pos]
		pos++
		if c&0x80 != 0 {
			// copy from base
			var cpOff, cpSize uint64
			for i := 0; i < 4; i++ {
				if c&(1<<uint(i)) != 0 {
					cpOff |= uint64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if c&(1<<uint(4+i)) != 0 {
					cpSize |= uint64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else {
			// insert literal
			n := int(c & 0x7f)
			out = append(out, delta[pos:pos+n]...)
			pos += n
		}
	}
	return out, nil
}

var _ = sha1.Sum
