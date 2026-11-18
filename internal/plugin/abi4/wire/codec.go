package wire

import "encoding/binary"

// IN and OUT share the header and sorted section table but have different
// rules for section types and locations. Keep structural validation here.
const (
	HeaderLen     = 24
	SectionStride = 16
	MaxSections   = 8
	MaxEntries    = 8192
	MaxPayload    = 1 << 30
	MaxU32        = 1<<32 - 1

	SectionBlob      = 1
	SectionIndex     = 2
	SectionStartLine = 3
	SectionConnInfo  = 4
	SectionState     = 5
	SectionMutation  = 6
)

type SectionView struct {
	Kind, Off, Size int
	Count           uint32
}

func (s SectionView) Bytes(win []byte) []byte {
	end := s.Off + s.Size
	return win[s.Off:end:end]
}

type SectionTable struct {
	Total, ArenaOff, ArenaEnd int
	N                         int
	Sections                  [MaxSections]SectionView
}

func within(off, length, base, limit uint64) bool {
	if limit < base || off < base || off > limit {
		return false
	}
	return length <= limit-off
}

func ReadSectionTable(win []byte, magic uint32) (SectionTable, bool) {
	le := binary.LittleEndian
	if len(win) < HeaderLen || le.Uint32(win) != magic || le.Uint16(win[4:]) != 1 ||
		le.Uint16(win[6:]) != HeaderLen || le.Uint16(win[14:]) != SectionStride {
		return SectionTable{}, false
	}
	total := uint64(le.Uint32(win[8:]))
	n := int(le.Uint16(win[12:]))
	arena := uint64(le.Uint32(win[16:]))
	arenaSize := uint64(le.Uint32(win[20:]))
	tableEnd := uint64(HeaderLen + n*SectionStride)
	if n > MaxSections || total%8 != 0 || total > uint64(len(win)) ||
		arena%8 != 0 || !within(arena, arenaSize, tableEnd, total) {
		return SectionTable{}, false
	}
	table := SectionTable{Total: int(total), ArenaOff: int(arena), ArenaEnd: int(arena + arenaSize), N: n} // #nosec G115 -- table and arena fit in len(win)
	prev := 0
	for i := range n {
		w := win[HeaderLen+i*SectionStride:]
		kind := int(le.Uint16(w))
		off, size := uint64(le.Uint32(w[4:])), uint64(le.Uint32(w[8:]))
		if kind <= prev || le.Uint16(w[2:]) != 0 || off%4 != 0 || size > MaxPayload ||
			!within(off, size, tableEnd, total) {
			return SectionTable{}, false
		}
		s := SectionView{Kind: kind, Off: int(off), Size: int(size), Count: le.Uint32(w[12:])} // #nosec G115 -- span fits within total and len(win)
		for _, other := range table.Sections[:i] {
			if s.Size != 0 && other.Size != 0 && s.Off < other.Off+other.Size && other.Off < s.Off+s.Size {
				return SectionTable{}, false
			}
		}
		table.Sections[i] = s
		prev = kind
	}
	return table, true
}

// The layout has been checked before serialization (including wire sizes).
func WriteEnvelopeHeader(dst []byte, magic uint32, total, count, arenaOff, arenaLen int) {
	le := binary.LittleEndian
	le.PutUint32(dst, magic)
	le.PutUint16(dst[4:], 1)
	le.PutUint16(dst[6:], HeaderLen)
	le.PutUint32(dst[8:], uint32(total))  // #nosec G115 -- validated envelope size
	le.PutUint16(dst[12:], uint16(count)) // #nosec G115 -- at most maxSections
	le.PutUint16(dst[14:], SectionStride)
	le.PutUint32(dst[16:], uint32(arenaOff)) // #nosec G115 -- validated arena offset
	le.PutUint32(dst[20:], uint32(arenaLen)) // #nosec G115 -- arena fits within total
}

// The writer has already cleared reserved fields and checked all capacities.
func WriteSection(dst []byte, i, kind, off, size, count int) {
	w := dst[HeaderLen+i*SectionStride:]
	le := binary.LittleEndian
	le.PutUint16(w, uint16(kind))       // #nosec G115 -- known section identifier
	le.PutUint32(w[4:], uint32(off))    // #nosec G115 -- callers pass a validated wire offset
	le.PutUint32(w[8:], uint32(size))   // #nosec G115 -- callers pass a validated wire length
	le.PutUint32(w[12:], uint32(count)) // #nosec G115 -- section counts are bounded
}
