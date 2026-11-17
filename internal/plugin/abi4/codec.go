package abi4

import "encoding/binary"

// IN and OUT share the header and sorted section table but have different
// rules for section types and locations. Keep structural validation here.
type sectionView struct {
	kind, off, size int
	count           uint32
}

func (s sectionView) bytes(win []byte) []byte {
	end := s.off + s.size
	return win[s.off:end:end]
}

type sectionTable struct {
	total, arenaOff, arenaEnd int
	n                         int
	sections                  [maxSections]sectionView
}

func readSectionTable(win []byte, magic uint32) (sectionTable, bool) {
	le := binary.LittleEndian
	if len(win) < hdrLen || le.Uint32(win) != magic || le.Uint16(win[4:]) != 1 ||
		le.Uint16(win[6:]) != hdrLen || le.Uint16(win[14:]) != secStride {
		return sectionTable{}, false
	}
	total := uint64(le.Uint32(win[8:]))
	n := int(le.Uint16(win[12:]))
	arena := uint64(le.Uint32(win[16:]))
	arenaSize := uint64(le.Uint32(win[20:]))
	tableEnd := uint64(hdrLen + n*secStride)
	if n > maxSections || total%8 != 0 || total > uint64(len(win)) ||
		arena%8 != 0 || !within(arena, arenaSize, tableEnd, total) {
		return sectionTable{}, false
	}
	table := sectionTable{total: int(total), arenaOff: int(arena), arenaEnd: int(arena + arenaSize), n: n} // #nosec G115 -- table and arena fit in len(win)
	prev := 0
	for i := range n {
		w := win[hdrLen+i*secStride:]
		kind := int(le.Uint16(w))
		off, size := uint64(le.Uint32(w[4:])), uint64(le.Uint32(w[8:]))
		if kind <= prev || le.Uint16(w[2:]) != 0 || off%4 != 0 || size > maxPayload ||
			!within(off, size, tableEnd, total) {
			return sectionTable{}, false
		}
		s := sectionView{kind: kind, off: int(off), size: int(size), count: le.Uint32(w[12:])} // #nosec G115 -- span fits within total and len(win)
		for _, other := range table.sections[:i] {
			if s.size != 0 && other.size != 0 && s.off < other.off+other.size && other.off < s.off+s.size {
				return sectionTable{}, false
			}
		}
		table.sections[i] = s
		prev = kind
	}
	return table, true
}

// The layout has been checked before serialization (including wire sizes).
func writeEnvelopeHeader(dst []byte, magic uint32, total, count, arenaOff, arenaLen int) {
	le := binary.LittleEndian
	le.PutUint32(dst, magic)
	le.PutUint16(dst[4:], 1)
	le.PutUint16(dst[6:], hdrLen)
	le.PutUint32(dst[8:], uint32(total))  // #nosec G115 -- validated envelope size
	le.PutUint16(dst[12:], uint16(count)) // #nosec G115 -- at most maxSections
	le.PutUint16(dst[14:], secStride)
	le.PutUint32(dst[16:], uint32(arenaOff)) // #nosec G115 -- validated arena offset
	le.PutUint32(dst[20:], uint32(arenaLen)) // #nosec G115 -- arena fits within total
}

// The writer has already cleared reserved fields and checked all capacities.
func writeSection(dst []byte, i, kind, off, size, count int) {
	w := dst[hdrLen+i*secStride:]
	le := binary.LittleEndian
	le.PutUint16(w, uint16(kind))       // #nosec G115 -- known section identifier
	le.PutUint32(w[4:], uint32(off))    // #nosec G115 -- callers pass a validated wire offset
	le.PutUint32(w[8:], uint32(size))   // #nosec G115 -- callers pass a validated wire length
	le.PutUint32(w[12:], uint32(count)) // #nosec G115 -- section counts are bounded
}
