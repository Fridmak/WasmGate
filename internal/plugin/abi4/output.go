package abi4

import "errors"

// Output is the bounded result envelope written by a guest into the OUT
// window. A mutation replaces the input block; state is the next slot value.
// Neither effect is applied until the caller has validated the allow result.
type Output struct {
	Mutation        []byte
	State           []byte
	MutationPresent bool
	StatePresent    bool
}

const magicOut uint32 = 0x5747344f
const secMutation = 6

var (
	errOutputCorrupt = errors.New("output envelope: corrupt")
	errOutputFull    = errors.New("output envelope: window too small")
)

// WriteOutput serializes a bounded OUT envelope, zeroing its padding and
// reserved fields. Empty output is represented by a valid zero-section envelope.
func WriteOutput(dst []byte, out Output) (int, error) {
	if len(out.State) > maxPayload || len(out.Mutation) > maxPayload-len(out.State) {
		return 0, errTooLarge
	}
	var sections [2]sectionView
	n := 0
	if out.StatePresent || len(out.State) != 0 {
		sections[n] = sectionView{kind: secState, size: len(out.State)}
		n++
	}
	if out.MutationPresent || len(out.Mutation) != 0 {
		sections[n] = sectionView{kind: secMutation, size: len(out.Mutation)}
		n++
	}
	arena := align8(hdrLen + n*secStride)
	end := arena
	for i := range n {
		sections[i].off = align4(end)
		end = sections[i].off + sections[i].size
	}
	total := align8(end)
	if total > len(dst) {
		return 0, errOutputFull
	}
	clear(dst[:total])
	writeEnvelopeHeader(dst, magicOut, total, n, arena, end-arena)
	for i, s := range sections[:n] {
		writeSection(dst, i, s.kind, s.off, s.size, 0)
		payload := out.State
		if s.kind == secMutation {
			payload = out.Mutation
		}
		copy(s.bytes(dst), payload)
	}
	return total, nil
}

// ParseOutput validates one OUT envelope and returns zero-copy payload views.
// Callers must consume the views before the guest instance is reused.
func ParseOutput(win []byte) (Output, error) {
	table, ok := readSectionTable(win, magicOut)
	if !ok {
		return Output{}, errOutputCorrupt
	}
	out := Output{}
	for _, s := range table.sections[:table.n] {
		if s.count != 0 || s.off < table.arenaOff || s.off+s.size > table.arenaEnd {
			return Output{}, errOutputCorrupt
		}
		switch s.kind {
		case secState:
			out.State = s.bytes(win)
			out.StatePresent = true
		case secMutation:
			out.Mutation = s.bytes(win)
			out.MutationPresent = true
		default:
			return Output{}, errOutputCorrupt
		}
	}
	return out, nil
}
