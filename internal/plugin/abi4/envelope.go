package abi4

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// Envelope layout (ABI v4.0, fixed):
//
//	[0 .. 24)            header
//	[24 .. 24+secs*16)   section table, sorted by type
//	arena                blob and state payloads
//	start line (24)      optional
//	conn info (80)       optional
//	header index (n*16)  optional
//
// SNI and ALPN are views into the blob; they are not separate arena payloads.
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
	maxPayload = 1 << 30 // hard cap for one envelope payload
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
// during building and are rebased to the window by Write. Parsed entries carry
// absolute offsets from the envelope base.
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
func planLayout(n, blobLen, stateLen int, withStart, withConn, withState bool) (Layout, error) {
	if n < 0 || blobLen < 0 || stateLen < 0 || n > maxEntries {
		return Layout{}, errBadCount
	}
	if blobLen > maxPayload || stateLen > maxPayload-blobLen {
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
	if withState {
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
// Block; Write rebases them into the window. A nil State omits the section.
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
	l, err := planLayout(len(idx), len(block), len(state), start != nil, conn != nil, state != nil)
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
	writeEnvelopeHeader(dst, magicIn, l.Total, l.Sections, l.ArenaOff, l.ArenaLen)

	// Section table, ascending by type (ids are assigned in that order).
	sec := 0
	putSection := func(typ int, off, ln, count int) {
		writeSection(dst, sec, typ, off, ln, count)
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
	if state != nil {
		putSection(secState, l.StateOff, len(state), 0)
	}

	// Fixed structs.
	if start != nil {
		w := dst[l.StartOff:]
		le.PutUint32(w[0:], uint32(l.ArenaOff)+start.MethodOff) // #nosec G115 -- layout total is bounded by maxU32
		le.PutUint32(w[4:], uint32(l.ArenaOff)+start.PathOff)   // #nosec G115 -- layout total is bounded by maxU32
		le.PutUint32(w[8:], uint32(l.ArenaOff)+start.ProtoOff)  // #nosec G115 -- layout total is bounded by maxU32
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
		le.PutUint32(w[48:], uint32(l.ArenaOff)+conn.SNIOff) // #nosec G115 -- layout total is bounded by maxU32
		le.PutUint32(w[52:], conn.SNILen)
		le.PutUint32(w[56:], uint32(l.ArenaOff)+conn.ALPNOff) // #nosec G115 -- layout total is bounded by maxU32
		le.PutUint32(w[60:], conn.ALPNLen)
		copy(w[64:80], conn.UpstreamIP[:])
	}

	// Index: rebase once, no per-entry branches.
	base := uint32(l.ArenaOff) // #nosec G115 -- layout total is bounded by maxU32
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
	return within(uint64(off), uint64(ln), uint64(arenaOff), uint64(arenaOff+blobLen)) // #nosec G115 -- validated blob span within the window
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

// StartLine is a fixed-struct payload with offsets relative to the blob when
// building an event. For response headers, the protocol fields point at the
// HTTP version, the path fields at the three-digit status, and the method
// fields at the reason phrase. That phase uses the same wire layout as a request.
type StartLine struct {
	MethodOff, PathOff, ProtoOff uint32
	MethodLen, PathLen, ProtoLen uint16
	TargetForm, Flags            byte
}

// ConnInfo describes optional connection and upstream metadata in an envelope.
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

// Parsed is a validated, zero-copy view of an IN envelope.
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
	table, ok := readSectionTable(win, magicIn)
	if !ok || table.n == 0 || table.sections[0].kind != secBlob {
		return nil, errCorrupt
	}

	p := &Parsed{Total: table.total, SectionIDs: make([]int, 0, table.n)}
	for _, s := range table.sections[:table.n] {
		p.SectionIDs = append(p.SectionIDs, s.kind)
		switch s.kind {
		case secBlob:
			if s.count != 0 || s.off != table.arenaOff || s.off+s.size > table.arenaEnd {
				return nil, errCorrupt
			}
			p.Blob = s.bytes(win)
		case secState:
			if s.count != 0 || s.off < table.arenaOff+len(p.Blob) || s.off+s.size > table.arenaEnd {
				return nil, errCorrupt
			}
			p.State = s.bytes(win)
		case secIndex:
			if s.off < table.arenaEnd || s.count == 0 || s.count > maxEntries || s.size != int(s.count)*entLen {
				return nil, errCorrupt
			}
			p.Index = make([]Ent, int(s.count))
			for j := range p.Index {
				e := win[s.off+j*entLen:]
				p.Index[j] = Ent{
					NameOff: le.Uint32(e[0:]), ValOff: le.Uint32(e[4:]),
					NameLen: le.Uint16(e[8:]), ValLen: le.Uint16(e[10:]),
					Aux: le.Uint32(e[12:]),
				}
				if p.Index[j].NameLen == 0 ||
					!withinBlob(p.Index[j].NameOff, uint32(p.Index[j].NameLen), table.arenaOff, len(p.Blob)) ||
					!withinBlob(p.Index[j].ValOff, uint32(p.Index[j].ValLen), table.arenaOff, len(p.Blob)) {
					return nil, errCorrupt
				}
			}
		case secStart:
			if s.count != 0 || s.size != startLineLen || s.off < table.arenaEnd {
				return nil, errCorrupt
			}
			w := s.bytes(win)
			s := &StartLine{
				MethodOff: le.Uint32(w[0:]), PathOff: le.Uint32(w[4:]), ProtoOff: le.Uint32(w[8:]),
				MethodLen: le.Uint16(w[12:]), PathLen: le.Uint16(w[14:]), ProtoLen: le.Uint16(w[16:]),
				TargetForm: w[18], Flags: w[19],
			}
			if !validStart(s, table.arenaOff, len(p.Blob)) {
				return nil, errCorrupt
			}
			p.Start = s
		case secConn:
			if s.count != 0 || s.size != connInfoLen || s.off < table.arenaEnd {
				return nil, errCorrupt
			}
			w := s.bytes(win)
			c := &ConnInfo{Flags: le.Uint32(w[0:]), Family: w[4]}
			copy(c.RemoteIP[:], w[8:24])
			copy(c.LocalIP[:], w[24:40])
			c.RemotePort, c.LocalPort, c.UpstreamPort = le.Uint16(w[40:]), le.Uint16(w[42:]), le.Uint16(w[44:])
			c.SNIOff, c.SNILen = le.Uint32(w[48:]), le.Uint32(w[52:])
			c.ALPNOff, c.ALPNLen = le.Uint32(w[56:]), le.Uint32(w[60:])
			copy(c.UpstreamIP[:], w[64:80])
			if !withinOptionalBlob(c.SNIOff, c.SNILen, table.arenaOff, len(p.Blob)) ||
				!withinOptionalBlob(c.ALPNOff, c.ALPNLen, table.arenaOff, len(p.Blob)) {
				return nil, errCorrupt
			}
			p.Conn = c
		default:
			// Unknown sections are skipped by design (forward compatibility).
		}
	}
	return p, nil
}

// Header returns the first value for a name using a case-insensitive linear
// scan, matching the reference reader. Out-of-range views fail closed instead
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

// SplitHeaders indexes a complete CRLF-terminated HTTP/1 request head. All
// offsets are relative to block. It never silently drops headers or allocates.
func SplitHeaders(block []byte, idx []Ent) (int, StartLine, error) {
	return splitHeaders(block, idx, splitStartLine)
}

// SplitResponseHeaders indexes a complete HTTP/1 response head. See StartLine
// for the response's version, status, and reason phrase field mapping.
func SplitResponseHeaders(block []byte, idx []Ent) (int, StartLine, error) {
	return splitHeaders(block, idx, splitStatusLine)
}

func splitHeaders(block []byte, idx []Ent, splitLine func([]byte) (StartLine, error)) (n int, sl StartLine, err error) {
	if len(block) > maxPayload {
		return 0, StartLine{}, errTooLarge
	}
	lineEnd := bytes.IndexByte(block, '\n')
	if lineEnd < 1 || block[lineEnd-1] != '\r' {
		return 0, StartLine{}, errMalformedHeader
	}
	sl, err = splitLine(block[:lineEnd-1])
	if err != nil {
		return 0, StartLine{}, err
	}

	for pos := lineEnd + 1; pos < len(block); {
		start := pos
		lineEnd := bytes.IndexByte(block[start:], '\n')
		if lineEnd < 1 || block[start+lineEnd-1] != '\r' {
			return 0, StartLine{}, errMalformedHeader
		}
		pos += lineEnd + 1
		if lineEnd == 1 {
			if pos != len(block) {
				return 0, StartLine{}, errMalformedHeader
			}
			return n, sl, nil
		}
		if n >= len(idx) {
			return 0, StartLine{}, errTooManyHeaders
		}
		line := block[start : pos-2]
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
			NameOff: uint32(start), NameLen: uint16(nameLen), // #nosec G115 -- block length is maxPayload and lengths fit wire fields
			ValOff: uint32(start + vs), ValLen: uint16(valLen), // #nosec G115 -- block length is maxPayload and lengths fit wire fields
		}
		n++
	}
	return 0, StartLine{}, errMalformedHeader
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
	if !validFieldName(line[:sp1]) || !isHTTP1(line[sp2+1:]) {
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

func isHTTP1(version []byte) bool {
	return string(version) == "HTTP/1.1" || string(version) == "HTTP/1.0"
}

func splitStatusLine(line []byte) (StartLine, error) {
	if len(line) < 12 || !isHTTP1(line[:8]) || line[8] != ' ' ||
		line[9] < '1' || line[9] > '5' || line[10] < '0' || line[10] > '9' || line[11] < '0' || line[11] > '9' {
		return StartLine{}, errMalformedHeader
	}
	reason := len(line)
	if len(line) > 12 {
		if line[12] != ' ' || !validFieldValue(line[13:]) {
			return StartLine{}, errMalformedHeader
		}
		reason = 13
	}
	reasonLen := len(line) - reason
	if reasonLen > maxU16 {
		return StartLine{}, errTooLarge
	}
	return StartLine{ProtoLen: 8, PathOff: 9, PathLen: 3, MethodOff: uint32(reason), MethodLen: uint16(reasonLen)}, nil // #nosec G115 -- reason length checked above
}
