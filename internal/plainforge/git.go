package plainforge

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	objCommit   = 1
	objTree     = 2
	objBlob     = 3
	objTag      = 4
	objOfsDelta = 6
	objRefDelta = 7

	// Limits on unpacked Git data so a broken or oversized repository cannot
	// push Chartroom past its 30 MiB container budget.
	maxObjectBytes = 4 << 20
	maxPackBytes   = 16 << 20
	maxPackEntries = 16384
	maxPktLine     = 65520
)

type oid [20]byte

var zeroOID oid

func (o oid) isZero() bool   { return o == zeroOID }
func (o oid) String() string { return hex.EncodeToString(o[:]) }

func parseOID(s string) (oid, error) {
	var o oid
	if len(s) != 40 {
		return o, ErrCorrupt
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return o, ErrCorrupt
		}
	}
	_, err := hex.Decode(o[:], []byte(s))
	if err != nil {
		return zeroOID, ErrCorrupt
	}
	return o, nil
}

type gitObject struct {
	typ  int
	data []byte
}

func typeName(typ int) (string, bool) {
	switch typ {
	case objCommit:
		return "commit", true
	case objTree:
		return "tree", true
	case objBlob:
		return "blob", true
	case objTag:
		return "tag", true
	}
	return "", false
}

func hashObject(typ int, data []byte) (oid, error) {
	name, ok := typeName(typ)
	if !ok {
		return zeroOID, ErrCorrupt
	}
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "%s %d\x00", name, len(data))
	_, _ = h.Write(data)
	var out oid
	copy(out[:], h.Sum(nil))
	return out, nil
}

// treeEntry is one row inside a raw Git tree object.
type treeEntry struct {
	mode string
	name string
	id   oid
}

// treeSortKey implements Git's base_name_compare: directory entries ("40000")
// sort as though their name ended with '/'.
func treeSortKey(e treeEntry) string {
	if e.mode == "40000" {
		return e.name + "/"
	}
	return e.name
}

func validEntryMode(mode string) bool {
	switch mode {
	case "40000", "100644", "100755", "120000", "160000":
		return true
	}
	return false
}

func parseTree(data []byte) ([]treeEntry, error) {
	var entries []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp <= 0 {
			return nil, ErrCorrupt
		}
		mode := string(data[:sp])
		// Normalise zero-padded "040000" if an external tool ever wrote one,
		// while rejecting unknown modes.
		if mode == "040000" {
			mode = "40000"
		}
		if !validEntryMode(mode) {
			return nil, ErrCorrupt
		}
		rest := data[sp+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul <= 0 || len(rest) < nul+1+20 {
			return nil, ErrCorrupt
		}
		name := string(rest[:nul])
		if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
			return nil, ErrCorrupt
		}
		var id oid
		copy(id[:], rest[nul+1:nul+21])
		entries = append(entries, treeEntry{mode: mode, name: name, id: id})
		data = rest[nul+21:]
	}
	return entries, nil
}

func formatTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		buf.Write(e.id[:])
	}
	return buf.Bytes()
}

func parseCommitTree(data []byte) (oid, error) {
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return zeroOID, ErrCorrupt
	}
	line := string(data[:nl])
	hexID, ok := strings.CutPrefix(line, "tree ")
	if !ok {
		return zeroOID, ErrCorrupt
	}
	return parseOID(hexID)
}

func formatCommit(treeID, parentID oid, at time.Time, message string) []byte {
	cleanMsg := strings.Map(func(r rune) rune {
		if r == '\n' || (r >= 0x20 && r != 0x7f) {
			return r
		}
		return -1
	}, strings.TrimSpace(message))
	if cleanMsg == "" {
		cleanMsg = "catalog: update"
	}
	stamp := strconv.FormatInt(at.UTC().Unix(), 10) + " +0000"
	ident := "Chartroom <chartroom@clearedstack.internal> " + stamp
	var buf bytes.Buffer
	buf.WriteString("tree " + treeID.String() + "\n")
	if !parentID.isZero() {
		buf.WriteString("parent " + parentID.String() + "\n")
	}
	buf.WriteString("author " + ident + "\n")
	buf.WriteString("committer " + ident + "\n\n")
	buf.WriteString(cleanMsg + "\n")
	return buf.Bytes()
}

func cleanRelPath(path string) ([]string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || strings.HasPrefix(trimmed, "/") || strings.HasSuffix(trimmed, "/") {
		return nil, ErrRefused
	}
	parts := strings.Split(trimmed, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || p == ".git" {
			return nil, ErrRefused
		}
		for i := 0; i < len(p); i++ {
			if p[i] < 0x20 || p[i] == 0x7f {
				return nil, ErrRefused
			}
		}
	}
	return parts, nil
}

// --- pkt-line framing ---

func writePktString(w io.Writer, s string) {
	_, _ = fmt.Fprintf(w, "%04x%s", len(s)+4, s)
}

func writePktFlush(w io.Writer) {
	_, _ = io.WriteString(w, "0000")
}

// readPktLine reads one pkt-line from r. flush is true on "0000".
func readPktLine(r io.Reader) (line []byte, flush bool, err error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, false, err
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
	if err != nil {
		return nil, false, ErrCorrupt
	}
	switch {
	case n == 0:
		return nil, true, nil
	case n < 4 || n > maxPktLine:
		return nil, false, ErrCorrupt
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, false, ErrCorrupt
	}
	return payload, false, nil
}

// --- PACKv2 reader and writer ---

// packStream hashes every byte read up to the 20-byte pack checksum and
// implements io.ByteReader so compress/zlib never over-reads into the next
// entry or the checksum trailer.
type packStream struct {
	br *bufio.Reader
	h  hash.Hash
	n  int64
}

func (ps *packStream) Read(p []byte) (int, error) {
	n, err := ps.br.Read(p)
	if n > 0 {
		_, _ = ps.h.Write(p[:n])
		ps.n += int64(n)
	}
	return n, err
}

func (ps *packStream) ReadByte() (byte, error) {
	b, err := ps.br.ReadByte()
	if err == nil {
		_, _ = ps.h.Write([]byte{b})
		ps.n++
	}
	return b, err
}

type pendingDelta struct {
	offset  int64
	baseOff int64
	baseOID oid
	ofs     bool
	delta   []byte
}

func unpackPack(r io.Reader, known map[oid]gitObject) (map[oid]gitObject, error) {
	br := bufio.NewReaderSize(r, 32<<10)
	ps := &packStream{br: br, h: sha1.New()}

	var head [12]byte
	if _, err := io.ReadFull(ps, head[:]); err != nil {
		return nil, ErrCorrupt
	}
	if string(head[:4]) != "PACK" || binary.BigEndian.Uint32(head[4:8]) != 2 {
		return nil, ErrCorrupt
	}
	count := binary.BigEndian.Uint32(head[8:12])
	if count > maxPackEntries {
		return nil, ErrCorrupt
	}

	out := make(map[oid]gitObject, int(count))
	byOffset := make(map[int64]oid, int(count))
	var deltas []pendingDelta
	var totalBytes int64

	lookup := func(id oid) (gitObject, bool) {
		if o, ok := out[id]; ok {
			return o, true
		}
		if known != nil {
			if o, ok := known[id]; ok {
				return o, true
			}
		}
		return gitObject{}, false
	}

	for i := uint32(0); i < count; i++ {
		entryOffset := ps.n
		b, err := ps.ReadByte()
		if err != nil {
			return nil, ErrCorrupt
		}
		typ := int((b >> 4) & 7)
		size := int64(b & 0x0f)
		for shift := uint(4); b&0x80 != 0; shift += 7 {
			if shift > 28 {
				return nil, ErrCorrupt
			}
			b, err = ps.ReadByte()
			if err != nil {
				return nil, ErrCorrupt
			}
			size |= int64(b&0x7f) << shift
		}
		if size < 0 || size > maxObjectBytes {
			return nil, ErrCorrupt
		}

		var baseOff int64
		var baseID oid
		switch typ {
		case objCommit, objTree, objBlob, objTag:
		case objOfsDelta:
			b, err = ps.ReadByte()
			if err != nil {
				return nil, ErrCorrupt
			}
			off := int64(b & 0x7f)
			for b&0x80 != 0 {
				b, err = ps.ReadByte()
				if err != nil {
					return nil, ErrCorrupt
				}
				off = ((off + 1) << 7) | int64(b&0x7f)
				if off > entryOffset {
					return nil, ErrCorrupt
				}
			}
			if off <= 0 || off > entryOffset {
				return nil, ErrCorrupt
			}
			baseOff = entryOffset - off
		case objRefDelta:
			if _, err := io.ReadFull(ps, baseID[:]); err != nil {
				return nil, ErrCorrupt
			}
		default:
			return nil, ErrCorrupt
		}

		zr, err := zlib.NewReader(ps)
		if err != nil {
			return nil, ErrCorrupt
		}
		data, err := io.ReadAll(io.LimitReader(zr, size+1))
		closeErr := zr.Close()
		if err != nil || closeErr != nil || int64(len(data)) != size {
			return nil, ErrCorrupt
		}
		totalBytes += int64(len(data))
		if totalBytes > maxPackBytes {
			return nil, ErrCorrupt
		}

		if typ == objOfsDelta || typ == objRefDelta {
			deltas = append(deltas, pendingDelta{
				offset:  entryOffset,
				baseOff: baseOff,
				baseOID: baseID,
				ofs:     typ == objOfsDelta,
				delta:   data,
			})
			continue
		}
		id, err := hashObject(typ, data)
		if err != nil {
			return nil, err
		}
		out[id] = gitObject{typ: typ, data: data}
		byOffset[entryOffset] = id
	}

	wantSum := ps.h.Sum(nil)
	var gotSum [20]byte
	if _, err := io.ReadFull(br, gotSum[:]); err != nil || !bytes.Equal(wantSum, gotSum[:]) {
		return nil, ErrCorrupt
	}
	if _, err := br.ReadByte(); err != io.EOF {
		return nil, ErrCorrupt
	}

	for len(deltas) > 0 {
		progressed := false
		var rem []pendingDelta
		for _, d := range deltas {
			baseID := d.baseOID
			if d.ofs {
				var ok bool
				baseID, ok = byOffset[d.baseOff]
				if !ok {
					rem = append(rem, d)
					continue
				}
			}
			baseObj, ok := lookup(baseID)
			if !ok {
				rem = append(rem, d)
				continue
			}
			rebuilt, err := applyDelta(baseObj.data, d.delta)
			if err != nil {
				return nil, err
			}
			totalBytes += int64(len(rebuilt))
			if totalBytes > maxPackBytes {
				return nil, ErrCorrupt
			}
			id, err := hashObject(baseObj.typ, rebuilt)
			if err != nil {
				return nil, err
			}
			out[id] = gitObject{typ: baseObj.typ, data: rebuilt}
			byOffset[d.offset] = id
			progressed = true
		}
		if !progressed {
			return nil, ErrCorrupt
		}
		deltas = rem
	}
	return out, nil
}

func readDeltaVarint(data []byte, pos *int) (int, error) {
	var v uint64
	var shift uint
	for {
		if *pos >= len(data) || shift > 28 {
			return 0, ErrCorrupt
		}
		b := data[*pos]
		*pos++
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	if v > maxObjectBytes {
		return 0, ErrCorrupt
	}
	return int(v), nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	srcSize, err := readDeltaVarint(delta, &pos)
	if err != nil || srcSize != len(base) {
		return nil, ErrCorrupt
	}
	dstSize, err := readDeltaVarint(delta, &pos)
	if err != nil {
		return nil, ErrCorrupt
	}
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		cmd := delta[pos]
		pos++
		switch {
		case cmd&0x80 != 0:
			var cpOff, cpSize int
			if cmd&0x01 != 0 {
				if pos >= len(delta) {
					return nil, ErrCorrupt
				}
				cpOff |= int(delta[pos])
				pos++
			}
			if cmd&0x02 != 0 {
				if pos >= len(delta) {
					return nil, ErrCorrupt
				}
				cpOff |= int(delta[pos]) << 8
				pos++
			}
			if cmd&0x04 != 0 {
				if pos >= len(delta) {
					return nil, ErrCorrupt
				}
				cpOff |= int(delta[pos]) << 16
				pos++
			}
			if cmd&0x08 != 0 {
				if pos >= len(delta) {
					return nil, ErrCorrupt
				}
				cpOff |= int(delta[pos]) << 24
				pos++
			}
			if cmd&0x10 != 0 {
				if pos >= len(delta) {
					return nil, ErrCorrupt
				}
				cpSize |= int(delta[pos])
				pos++
			}
			if cmd&0x20 != 0 {
				if pos >= len(delta) {
					return nil, ErrCorrupt
				}
				cpSize |= int(delta[pos]) << 8
				pos++
			}
			if cmd&0x40 != 0 {
				if pos >= len(delta) {
					return nil, ErrCorrupt
				}
				cpSize |= int(delta[pos]) << 16
				pos++
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff < 0 || cpSize < 0 || cpOff+cpSize > len(base) || len(out)+cpSize > dstSize {
				return nil, ErrCorrupt
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		case cmd > 0:
			n := int(cmd)
			if pos+n > len(delta) || len(out)+n > dstSize {
				return nil, ErrCorrupt
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		default:
			return nil, ErrCorrupt
		}
	}
	if len(out) != dstSize {
		return nil, ErrCorrupt
	}
	return out, nil
}

func writePack(objects []gitObject) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objects)))
	buf.Write(hdr[:])

	for _, o := range objects {
		size := len(o.data)
		first := byte((o.typ&7)<<4) | byte(size&0x0f)
		size >>= 4
		for size > 0 {
			buf.WriteByte(first | 0x80)
			first = byte(size & 0x7f)
			size >>= 7
		}
		buf.WriteByte(first)

		zw := zlib.NewWriter(&buf)
		if _, err := zw.Write(o.data); err != nil {
			_ = zw.Close()
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes(), nil
}
