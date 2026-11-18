package envelope

import (
	"encoding/binary"
	"errors"

	"wasmgate/internal/plugin/abi4/wire"
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
	startLineLen = 24
	connInfoLen  = 80
	entLen       = 16
)

// Local protocol aliases keep the envelope implementation and its focused
// tests readable while the shared section-table implementation lives in wire.
const (
	hdrLen      = wire.HeaderLen
	secStride   = wire.SectionStride
	maxSections = wire.MaxSections
	maxEntries  = wire.MaxEntries
	maxPayload  = wire.MaxPayload
	maxU32      = wire.MaxU32
	secBlob     = wire.SectionBlob
	secIndex    = wire.SectionIndex
	secStart    = wire.SectionStartLine
	secConn     = wire.SectionConnInfo
	secState    = wire.SectionState
)

const magicIn uint32 = 0x57473445 // "WG4E" little-endian

var (
	errWindowFull = errors.New("envelope: window too small")
	errBadCount   = errors.New("envelope: too many entries")
	errTooLarge   = errors.New("envelope: payload too large")
	errCorrupt    = errors.New("envelope: corrupt")
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
	if n < 0 || blobLen < 0 || stateLen < 0 || n > wire.MaxEntries {
		return Layout{}, errBadCount
	}
	if blobLen > wire.MaxPayload || stateLen > wire.MaxPayload-blobLen {
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
	l.TableEnd = wire.HeaderLen + l.Sections*wire.SectionStride
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
	if l.Total < 0 || uint64(l.Total) > wire.MaxU32 {
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
	wire.WriteEnvelopeHeader(dst, magicIn, l.Total, l.Sections, l.ArenaOff, l.ArenaLen)

	// Section table, ascending by type (ids are assigned in that order).
	sec := 0
	putSection := func(typ int, off, ln, count int) {
		wire.WriteSection(dst, sec, typ, off, ln, count)
		sec++
	}
	putSection(wire.SectionBlob, l.ArenaOff, len(block), 0)
	if len(idx) > 0 {
		putSection(wire.SectionIndex, l.IndexOff, len(idx)*entLen, len(idx))
	}
	if start != nil {
		putSection(wire.SectionStartLine, l.StartOff, startLineLen, 0)
	}
	if conn != nil {
		putSection(wire.SectionConnInfo, l.ConnOff, connInfoLen, 0)
	}
	if state != nil {
		putSection(wire.SectionState, l.StateOff, len(state), 0)
	}

	// Fixed structs.
	if start != nil {
		w := dst[l.StartOff:]
		le.PutUint32(w[0:], uint32(l.ArenaOff)+start.MethodOff) // #nosec G115 -- layout total is bounded by wire.MaxU32
		le.PutUint32(w[4:], uint32(l.ArenaOff)+start.PathOff)   // #nosec G115 -- layout total is bounded by wire.MaxU32
		le.PutUint32(w[8:], uint32(l.ArenaOff)+start.ProtoOff)  // #nosec G115 -- layout total is bounded by wire.MaxU32
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
		le.PutUint32(w[48:], uint32(l.ArenaOff)+conn.SNIOff) // #nosec G115 -- layout total is bounded by wire.MaxU32
		le.PutUint32(w[52:], conn.SNILen)
		le.PutUint32(w[56:], uint32(l.ArenaOff)+conn.ALPNOff) // #nosec G115 -- layout total is bounded by wire.MaxU32
		le.PutUint32(w[60:], conn.ALPNLen)
		copy(w[64:80], conn.UpstreamIP[:])
	}

	// Index: rebase once, no per-entry branches.
	base := uint32(l.ArenaOff) // #nosec G115 -- layout total is bounded by wire.MaxU32
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
	if len(block) > wire.MaxPayload {
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
