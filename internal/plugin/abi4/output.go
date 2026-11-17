package abi4

import (
	"encoding/binary"
	"errors"
)

// Output is the bounded result envelope written by a guest into the OUT
// window. A mutation replaces the input block; it is applied only after the
// guest returns an allow verdict. State is the next value of the connection's
// state slot and is committed together with the verdict.
type Output struct {
	Mutation        []byte
	State           []byte
	MutationPresent bool
	StatePresent    bool
}

const (
	magicOut        uint32 = 0x5747344f // "WG4O" little-endian
	secMutation            = 6
	MutationReplace        = 1
)

var (
	errOutputCorrupt = errors.New("output envelope: corrupt")
	errOutputFull    = errors.New("output envelope: window too small")
)

// WriteOutput serializes a bounded OUT envelope. The output window is cleared
// before writing so stale mutation/state bytes cannot leak across pooled guest
// instances. Empty output is represented by a valid zero-section envelope.
func WriteOutput(dst []byte, out Output) (int, error) {
	if len(out.Mutation) > maxPayload || len(out.State) > maxPayload {
		return 0, errTooLarge
	}
	sections := 0
	if out.StatePresent || len(out.State) > 0 {
		sections++
	}
	if out.MutationPresent || len(out.Mutation) > 0 {
		sections++
	}
	if sections > maxSections {
		return 0, errBadCount
	}
	tableEnd := hdrLen + sections*secStride
	arenaOff := align8(tableEnd)
	stateOff := arenaOff
	if len(out.State) > 0 {
		stateOff = arenaOff
	}
	mutationOff := align4(arenaOff + len(out.State))
	arenaLen := mutationOff - arenaOff + len(out.Mutation)
	total := align8(arenaOff + arenaLen)
	if total > len(dst) {
		return 0, errOutputFull
	}
	clear(dst[:total])
	le := binary.LittleEndian
	le.PutUint32(dst[0:], magicOut)
	le.PutUint16(dst[4:], 1)
	le.PutUint16(dst[6:], hdrLen)
	le.PutUint32(dst[8:], uint32(total))
	le.PutUint16(dst[12:], uint16(sections))
	le.PutUint16(dst[14:], secStride)
	le.PutUint32(dst[16:], uint32(arenaOff))
	le.PutUint32(dst[20:], uint32(arenaLen))

	sec := 0
	put := func(typ, off, ln int) {
		w := dst[hdrLen+sec*secStride:]
		le.PutUint16(w[0:], uint16(typ))
		le.PutUint32(w[4:], uint32(off))
		le.PutUint32(w[8:], uint32(ln))
		sec++
	}
	if out.StatePresent || len(out.State) > 0 {
		put(secState, stateOff, len(out.State))
		copy(dst[stateOff:], out.State)
	}
	if out.MutationPresent || len(out.Mutation) > 0 {
		put(secMutation, mutationOff, len(out.Mutation))
		copy(dst[mutationOff:], out.Mutation)
	}
	return total, nil
}

// ParseOutput validates one OUT envelope and returns zero-copy payload views.
func ParseOutput(win []byte) (Output, error) {
	le := binary.LittleEndian
	if len(win) < hdrLen || le.Uint32(win[0:]) != magicOut ||
		le.Uint16(win[4:]) != 1 || le.Uint16(win[6:]) != hdrLen ||
		le.Uint16(win[14:]) != secStride {
		return Output{}, errOutputCorrupt
	}
	total := uint64(le.Uint32(win[8:]))
	count := int(le.Uint16(win[12:]))
	arenaOff := uint64(le.Uint32(win[16:]))
	arenaLen := uint64(le.Uint32(win[20:]))
	if total < hdrLen || total > uint64(len(win)) || total%8 != 0 ||
		count > maxSections || arenaOff%8 != 0 {
		return Output{}, errOutputCorrupt
	}
	tableEnd := uint64(hdrLen) + uint64(count)*secStride
	if tableEnd > arenaOff || arenaOff > total || arenaLen > total-arenaOff {
		return Output{}, errOutputCorrupt
	}
	out := Output{}
	prev := 0
	var spans [2]struct{ start, end uint64 }
	spanN := 0
	for i := 0; i < count; i++ {
		w := win[hdrLen+i*secStride:]
		typ := int(le.Uint16(w[0:]))
		off := uint64(le.Uint32(w[4:]))
		ln := uint64(le.Uint32(w[8:]))
		if typ <= prev || (typ != secState && typ != secMutation) ||
			off < arenaOff || off > total || ln > total-off ||
			off+ln > arenaOff+arenaLen {
			return Output{}, errOutputCorrupt
		}
		if ln > 0 {
			for j := 0; j < spanN; j++ {
				if off < spans[j].end && spans[j].start < off+ln {
					return Output{}, errOutputCorrupt
				}
			}
			spans[spanN] = struct{ start, end uint64 }{off, off + ln}
			spanN++
		}
		prev = typ
		payload := win[int(off):int(off+ln)]
		switch typ {
		case secState:
			if out.StatePresent {
				return Output{}, errOutputCorrupt
			}
			out.State = payload
			out.StatePresent = true
		case secMutation:
			if out.MutationPresent {
				return Output{}, errOutputCorrupt
			}
			out.Mutation = payload
			out.MutationPresent = true
		}
	}
	return out, nil
}
