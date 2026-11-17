package abi4

import (
	"encoding/binary"
	"errors"
)

// Envelope layout (ABI v4.0, fixed):
//
//	[0 .. 24)            header
//	[24 .. 24+secs*16)   section table, sorted by type
//	arena                all variable-length payloads (blob, sni, alpn, state)
//	start line (24)      optional
//	conn info (80)       optional
//	header index (n*16)  optional
//
// All offsets are absolute from the window base. All fields are little-endian,
// fixed structs and the section table are 4-byte aligned, total_len is a
// multiple of 8. Unused table/header bytes are zero.
const (
	hdrLen    = 24
	secStride = 16

	secBlob  = 1
	secIndex = 2
	secStart = 3
	secConn  = 4
	secState = 5

	startLineLen = 24
	connInfoLen  = 80
	entLen       = 16

	maxSections = 8
	// The index is an AoS array inside the same window. 8192 entries is
	// already far beyond any legitimate HTTP header block and prevents a
	// malicious envelope from forcing a large host-side allocation.
	maxEntries = 8192
	maxPayload = 1 << 30
)

const (
	maxU16 = 1<<16 - 1
	maxU32 = 1<<32 - 1
)

const magicIn uint32 = 0x57473445 // "WG4E" little-endian

var (
	errWindowFull      = errors.New("envelope: window too small")
	errBadCount        = errors.New("envelope: too many entries")
	errTooLarge        = errors.New("envelope: payload too large")
	errCorrupt         = errors.New("envelope: corrupt")
	errMalformedHeader = errors.New("envelope: malformed header block")
	errTooManyHeaders  = errors.New("envelope: too many headers")
)

// Ent is one AoS header-index entry. Offsets are relative to the blob start
// during building and are rebased to the window by Write.
type Ent struct {
	NameOff, ValOff uint32
	NameLen, ValLen uint16
	Aux             uint32
}

// Layout is computed before a single byte is written, so the capacity check
// happens first.
type Layout struct {
	N, Sections        int
	TableEnd, ArenaOff int
	StateOff           int
	ArenaLen, Total    int
	StartOff, ConnOff  int
	IndexOff           int
	BlobLen, StateLen  int
}

func align4(n int) int { return (n + 3) &^ 3 }
func align8(n int) int { return (n + 7) &^ 7 }

// planLayout is the single source of truth for the arena and for total_len.
func planLayout(n, blobLen, stateLen int, withStart, withConn bool) (Layout, error) {
	if n < 0 || blobLen < 0 || stateLen < 0 || n > maxEntries {
		return Layout{}, errBadCount
	}
	if blobLen > maxPayload || stateLen > maxPayload || blobLen+stateLen > maxPayload {
		return Layout{}, errTooLarge
	}
	l := Layout{N: n, BlobLen: blobLen, StateLen: stateLen}
	l.Sections = 1 // blob
	if n > 0 {
		l.Sections++
	}
	if withStart {
		l.Sections++
	}
	if withConn {
		l.Sections++
	}
	if stateLen > 0 {
		l.Sections++
	}
	if l.Sections > maxSections {
		return Layout{}, errBadCount
	}
	l.TableEnd = hdrLen + l.Sections*secStride
	l.ArenaOff = align8(l.TableEnd)
	l.StateOff = align4(l.ArenaOff + blobLen)
	l.ArenaLen = l.StateOff - l.ArenaOff + stateLen
	p := align8(l.ArenaOff + l.ArenaLen)
	if withStart {
		l.StartOff = p
		p += startLineLen
	}
	if withConn {
		l.ConnOff = p
		p += connInfoLen
	}
	l.IndexOff = p
	l.Total = align8(l.IndexOff + n*entLen)
	if l.Total < 0 || uint64(l.Total) > maxU32 {
		return Layout{}, errTooLarge
	}
	return l, nil
}

// Event is one IN envelope. Offsets in Index, Start and Conn are relative to
// Block; Write rebases them into the window.
type Event struct {
	Block []byte
	Index []Ent
	Start *StartLine
	Conn  *ConnInfo
	State []byte
}

// Write builds one envelope into dst (the guest IN window).
//
// All inputs are validated and the capacity check is performed before the
// first store, so on error dst is untouched. This is important: a failed event
// must never leave a partially written envelope in a pooled instance.
func Write(dst []byte, ev Event) (int, error) {
	block, idx, start, conn, state := ev.Block, ev.Index, ev.Start, ev.Conn, ev.State
	if err := validateEvent(block, idx, start, conn); err != nil {
		return 0, err
	}
	l, err := planLayout(len(idx), len(block), len(state), start != nil, conn != nil)
	if err != nil {
		return 0, err
	}
	if l.Total > len(dst) {
		return 0, errWindowFull
	}
	// The contract requires unused header/table/padding bytes to be zero.
	clear(dst[:l.Total])

	le := binary.LittleEndian

	// Arena: the only copy of the payload(s).
	copy(dst[l.ArenaOff:], block)
	if len(state) > 0 {
		copy(dst[l.StateOff:], state)
	}

	// Header.
	le.PutUint32(dst[0:], magicIn)
	le.PutUint16(dst[4:], 1)
	le.PutUint16(dst[6:], hdrLen)
	le.PutUint32(dst[8:], uint32(l.Total))
	le.PutUint16(dst[12:], uint16(l.Sections))
	le.PutUint16(dst[14:], secStride)
	le.PutUint32(dst[16:], uint32(l.ArenaOff))
	le.PutUint32(dst[20:], uint32(l.ArenaLen))

	// Section table, ascending by type (ids are assigned in that order).
	sec := 0
	putSection := func(typ int, off, ln, count int) {
		w := dst[hdrLen+sec*secStride:]
		le.PutUint16(w[0:], uint16(typ))
		le.PutUint16(w[2:], 0)
		le.PutUint32(w[4:], uint32(off))
		le.PutUint32(w[8:], uint32(ln))
		le.PutUint32(w[12:], uint32(count))
		sec++
	}
	putSection(secBlob, l.ArenaOff, len(block), 0)
	if len(idx) > 0 {
		putSection(secIndex, l.IndexOff, len(idx)*entLen, len(idx))
	}
	if start != nil {
		putSection(secStart, l.StartOff, startLineLen, 0)
	}
	if conn != nil {
		putSection(secConn, l.ConnOff, connInfoLen, 0)
	}
	if len(state) > 0 {
		putSection(secState, l.StateOff, len(state), 0)
	}

	// Fixed structs.
	if start != nil {
		w := dst[l.StartOff:]
		le.PutUint32(w[0:], uint32(l.ArenaOff)+start.MethodOff)
		le.PutUint32(w[4:], uint32(l.ArenaOff)+start.PathOff)
		le.PutUint32(w[8:], uint32(l.ArenaOff)+start.ProtoOff)
		le.PutUint16(w[12:], start.MethodLen)
		le.PutUint16(w[14:], start.PathLen)
		le.PutUint16(w[16:], start.ProtoLen)
		w[18], w[19] = start.TargetForm, start.Flags
	}
	if conn != nil {
		w := dst[l.ConnOff:]
		le.PutUint32(w[0:], conn.Flags)
		w[4] = conn.Family
		copy(w[8:24], conn.RemoteIP[:])
		copy(w[24:40], conn.LocalIP[:])
		le.PutUint16(w[40:], conn.RemotePort)
		le.PutUint16(w[42:], conn.LocalPort)
		le.PutUint16(w[44:], conn.UpstreamPort)
		le.PutUint32(w[48:], uint32(l.ArenaOff)+conn.SNIOff)
		le.PutUint32(w[52:], conn.SNILen)
		le.PutUint32(w[56:], uint32(l.ArenaOff)+conn.ALPNOff)
		le.PutUint32(w[60:], conn.ALPNLen)
		copy(w[64:80], conn.UpstreamIP[:])
	}

	// Index: rebase once, no per-entry branches.
	base := uint32(l.ArenaOff)
	for i := range idx {
		w := dst[l.IndexOff+i*entLen:]
		le.PutUint32(w[0:], base+idx[i].NameOff)
		le.PutUint32(w[4:], base+idx[i].ValOff)
		le.PutUint16(w[8:], idx[i].NameLen)
		le.PutUint16(w[10:], idx[i].ValLen)
		le.PutUint32(w[12:], idx[i].Aux)
	}
	return l.Total, nil
}

func within(off, ln, base, limit uint64) bool {
	if limit < base || off < base || off > limit {
		return false
	}
	return ln <= limit-off
}

func withinBlob(off, ln uint32, arenaOff, blobLen int) bool {
	return within(uint64(off), uint64(ln), uint64(arenaOff), uint64(arenaOff+blobLen))
}

func withinOptionalBlob(off, ln uint32, arenaOff, blobLen int) bool {
	if ln == 0 {
		return true
	}
	return withinBlob(off, ln, arenaOff, blobLen)
}

func validStart(s *StartLine, arenaOff, blobLen int) bool {
	if s == nil {
		return true
	}
	return withinOptionalBlob(s.MethodOff, uint32(s.MethodLen), arenaOff, blobLen) &&
		withinOptionalBlob(s.PathOff, uint32(s.PathLen), arenaOff, blobLen) &&
		withinOptionalBlob(s.ProtoOff, uint32(s.ProtoLen), arenaOff, blobLen)
}

func validateEvent(block []byte, idx []Ent, start *StartLine, conn *ConnInfo) error {
	if len(block) > maxPayload {
		return errTooLarge
	}
	for i := range idx {
		e := &idx[i]
		if e.NameLen == 0 {
			return errCorrupt
		}
		if !withinBlob(e.NameOff, uint32(e.NameLen), 0, len(block)) ||
			!withinBlob(e.ValOff, uint32(e.ValLen), 0, len(block)) {
			return errCorrupt
		}
	}
	if !validStart(start, 0, len(block)) {
		return errCorrupt
	}
	if conn != nil &&
		(!withinOptionalBlob(conn.SNIOff, conn.SNILen, 0, len(block)) ||
			!withinOptionalBlob(conn.ALPNOff, conn.ALPNLen, 0, len(block))) {
		return errCorrupt
	}
	return nil
}

// StartLine and ConnInfo are the fixed-struct payloads. Offsets are relative
// to the blob.
type StartLine struct {
	MethodOff, PathOff, ProtoOff uint32
	MethodLen, PathLen, ProtoLen uint16
	TargetForm, Flags            byte
}

type ConnInfo struct {
	Flags        uint32
	Family       byte
	RemoteIP     [16]byte
	LocalIP      [16]byte
	RemotePort   uint16
	LocalPort    uint16
	UpstreamPort uint16
	SNIOff       uint32
	SNILen       uint32
	ALPNOff      uint32
	ALPNLen      uint32
	UpstreamIP   [16]byte
}

// Parsed is the reference reader used by tests and conformance checks.
type Parsed struct {
	Total      int
	Blob       []byte
	State      []byte
	Index      []Ent
	Start      *StartLine
	Conn       *ConnInfo
	SectionIDs []int
}

// Parse validates one envelope and returns zero-copy views into win. It rejects
// every out-of-bounds offset, overlapping/duplicate section and malformed
// fixed struct before returning a Parsed value.
func Parse(win []byte) (*Parsed, error) {
	le := binary.LittleEndian
	if len(win) < hdrLen || le.Uint32(win[0:]) != magicIn {
		return nil, errCorrupt
	}
	if le.Uint16(win[4:]) != 1 || le.Uint16(win[6:]) != hdrLen || le.Uint16(win[14:]) != secStride {
		return nil, errCorrupt
	}
	total := uint64(le.Uint32(win[8:]))
	count := int(le.Uint16(win[12:]))
	arenaOff := uint64(le.Uint32(win[16:]))
	arenaLen := uint64(le.Uint32(win[20:]))
	if total < hdrLen || total > uint64(len(win)) || total%8 != 0 {
		return nil, errCorrupt
	}
	if count == 0 || count > maxSections || arenaOff%8 != 0 {
		return nil, errCorrupt
	}
	tableEnd := uint64(hdrLen) + uint64(count)*secStride
	if tableEnd > arenaOff || arenaOff > total || arenaLen > total-arenaOff {
		return nil, errCorrupt
	}

	p := &Parsed{Total: int(total)}
	seen := 0
	blobSeen := false
	var spans [maxSections]struct{ start, end uint64 }
	spanN := 0
	for i := 0; i < count; i++ {
		w := win[hdrLen+i*secStride:]
		typ := int(le.Uint16(w[0:]))
		off := uint64(le.Uint32(w[4:]))
		ln := uint64(le.Uint32(w[8:]))
		cnt := uint64(le.Uint32(w[12:]))
		if typ <= 0 || typ <= seen || off < tableEnd || off%4 != 0 || off > total || ln > total-off {
			return nil, errCorrupt
		}
		if ln > 0 {
			for k := 0; k < spanN; k++ {
				if off < spans[k].end && spans[k].start < off+ln {
					return nil, errCorrupt
				}
			}
			spans[spanN] = struct{ start, end uint64 }{off, off + ln}
			spanN++
		}
		seen = typ
		p.SectionIDs = append(p.SectionIDs, typ)
		switch typ {
		case secBlob:
			if blobSeen || off != arenaOff || ln > arenaLen {
				return nil, errCorrupt
			}
			blobSeen = true
			p.Blob = win[int(off):int(off+ln)]
		case secState:
			if !blobSeen || off < arenaOff+uint64(len(p.Blob)) ||
				!within(off, ln, arenaOff, arenaOff+arenaLen) {
				return nil, errCorrupt
			}
			p.State = win[int(off):int(off+ln)]
		case secIndex:
			if off < arenaOff+arenaLen || cnt == 0 || cnt > maxEntries || ln != cnt*entLen {
				return nil, errCorrupt
			}
			p.Index = make([]Ent, int(cnt))
			for j := range p.Index {
				e := win[int(off)+j*entLen:]
				p.Index[j] = Ent{
					NameOff: le.Uint32(e[0:]), ValOff: le.Uint32(e[4:]),
					NameLen: le.Uint16(e[8:]), ValLen: le.Uint16(e[10:]),
					Aux: le.Uint32(e[12:]),
				}
				if p.Index[j].NameLen == 0 ||
					!withinBlob(p.Index[j].NameOff, uint32(p.Index[j].NameLen), int(arenaOff), len(p.Blob)) ||
					!withinBlob(p.Index[j].ValOff, uint32(p.Index[j].ValLen), int(arenaOff), len(p.Blob)) {
					return nil, errCorrupt
				}
			}
		case secStart:
			if ln != startLineLen || off < arenaOff+arenaLen {
				return nil, errCorrupt
			}
			w := win[int(off):]
			s := &StartLine{
				MethodOff: le.Uint32(w[0:]), PathOff: le.Uint32(w[4:]), ProtoOff: le.Uint32(w[8:]),
				MethodLen: le.Uint16(w[12:]), PathLen: le.Uint16(w[14:]), ProtoLen: le.Uint16(w[16:]),
				TargetForm: w[18], Flags: w[19],
			}
			if !validStart(s, int(arenaOff), len(p.Blob)) {
				return nil, errCorrupt
			}
			p.Start = s
		case secConn:
			if ln != connInfoLen || off < arenaOff+arenaLen {
				return nil, errCorrupt
			}
			w := win[int(off):]
			c := &ConnInfo{Flags: le.Uint32(w[0:]), Family: w[4]}
			copy(c.RemoteIP[:], w[8:24])
			copy(c.LocalIP[:], w[24:40])
			c.RemotePort, c.LocalPort, c.UpstreamPort = le.Uint16(w[40:]), le.Uint16(w[42:]), le.Uint16(w[44:])
			c.SNIOff, c.SNILen = le.Uint32(w[48:]), le.Uint32(w[52:])
			c.ALPNOff, c.ALPNLen = le.Uint32(w[56:]), le.Uint32(w[60:])
			copy(c.UpstreamIP[:], w[64:80])
			if !withinOptionalBlob(c.SNIOff, c.SNILen, int(arenaOff), len(p.Blob)) ||
				!withinOptionalBlob(c.ALPNOff, c.ALPNLen, int(arenaOff), len(p.Blob)) {
				return nil, errCorrupt
			}
			p.Conn = c
		default:
			// Unknown sections are skipped by design (forward compatibility).
		}
	}
	if !blobSeen {
		return nil, errCorrupt
	}
	return p, nil
}

// Header returns the first value for a name using the same algorithm as the
// guest SDK (case-insensitive, linear). Out-of-range views fail closed instead
// of panicking, even if the caller passes a short window.
func (p *Parsed) Header(win []byte, name string) ([]byte, bool) {
	for _, e := range p.Index {
		if int(e.NameLen) != len(name) {
			continue
		}
		if uint64(e.NameOff)+uint64(e.NameLen) > uint64(len(win)) ||
			uint64(e.ValOff)+uint64(e.ValLen) > uint64(len(win)) {
			return nil, false
		}
		k := win[int(e.NameOff) : int(e.NameOff)+int(e.NameLen)]
		eq := true
		for i := 0; i < len(k); i++ {
			a, b := k[i], name[i]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				eq = false
				break
			}
		}
		if eq {
			return win[int(e.ValOff) : int(e.ValOff)+int(e.ValLen)], true
		}
	}
	return nil, false
}

// SplitHeaders fills idx with name/value offsets relative to block and returns
// the count plus the request-line split. One pass, no allocation.
//
// It rejects malformed lines, control characters, header names that are not
// valid HTTP tokens and an index that is too small for the request. Rejecting
// is deliberate: silently dropping headers is a request-smuggling vector.
func SplitHeaders(block []byte, idx []Ent) (n int, sl StartLine, err error) {
	if len(block) > maxPayload {
		return 0, StartLine{}, errTooLarge
	}
	pos := 0
	for pos < len(block) && block[pos] != '\n' {
		pos++
	}
	lineEnd := pos
	if pos < len(block) {
		pos++
	}
	line := block[:lineEnd]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	sl, err = splitStartLine(line)
	if err != nil {
		return 0, StartLine{}, err
	}

	n = 0
	for pos < len(block) {
		start := pos
		for pos < len(block) && block[pos] != '\n' {
			pos++
		}
		end := pos
		if pos < len(block) {
			pos++
		}
		if end > start && block[end-1] == '\r' {
			end--
		}
		if end == start {
			break // final CRLF
		}
		if n >= len(idx) {
			return 0, StartLine{}, errTooManyHeaders
		}
		line := block[start:end]
		colon := -1
		for i := 0; i < len(line); i++ {
			if line[i] == ':' {
				colon = i
				break
			}
		}
		if colon <= 0 || !validFieldName(line[:colon]) {
			return 0, StartLine{}, errMalformedHeader
		}
		vs := colon + 1
		for vs < len(line) && (line[vs] == ' ' || line[vs] == '\t') {
			vs++
		}
		if !validFieldValue(line[vs:]) {
			return 0, StartLine{}, errMalformedHeader
		}
		nameLen := colon
		valLen := len(line) - vs
		if nameLen > maxU16 || valLen > maxU16 {
			return 0, StartLine{}, errTooLarge
		}
		idx[n] = Ent{
			NameOff: uint32(start), NameLen: uint16(nameLen),
			ValOff: uint32(start + vs), ValLen: uint16(valLen),
		}
		n++
	}
	return n, sl, nil
}

func validFieldName(name []byte) bool {
	if len(name) == 0 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
			continue
		case c == '!', c == '#', c == '$', c == '%', c == '&', c == '\'',
			c == '*', c == '+', c == '-', c == '.', c == '^', c == '_',
			c == '`', c == '|', c == '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validFieldValue(v []byte) bool {
	for _, c := range v {
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

func splitStartLine(line []byte) (StartLine, error) {
	sp1, sp2 := -1, -1
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c < 0x20 || c == 0x7f {
			return StartLine{}, errMalformedHeader
		}
		if c == ' ' {
			switch {
			case sp1 < 0:
				sp1 = i
			case sp2 < 0:
				sp2 = i
			default:
				return StartLine{}, errMalformedHeader
			}
		}
	}
	if sp1 <= 0 || sp2 <= sp1+1 || sp2 >= len(line)-1 {
		return StartLine{}, errMalformedHeader
	}
	methodLen, pathLen, protoLen := sp1, sp2-sp1-1, len(line)-sp2-1
	if methodLen > maxU16 || pathLen > maxU16 || protoLen > maxU16 {
		return StartLine{}, errTooLarge
	}
	return StartLine{
		MethodOff: 0, MethodLen: uint16(methodLen),
		PathOff: uint32(sp1 + 1), PathLen: uint16(pathLen),
		ProtoOff: uint32(sp2 + 1), ProtoLen: uint16(protoLen),
	}, nil
}
