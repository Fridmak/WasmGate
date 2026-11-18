package output

import (
	"errors"

	"wasmgate/internal/plugin/abi4/wire"
)

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

func align4(n int) int { return (n + 3) &^ 3 }
func align8(n int) int { return (n + 7) &^ 7 }

var (
	errOutputCorrupt  = errors.New("output envelope: corrupt")
	errOutputFull     = errors.New("output envelope: window too small")
	errOutputTooLarge = errors.New("output envelope: payload too large")
)

// WriteOutput serializes a bounded OUT envelope, zeroing its padding and
// reserved fields. Empty output is represented by a valid zero-section envelope.
func WriteOutput(dst []byte, out Output) (int, error) {
	if len(out.State) > wire.MaxPayload || len(out.Mutation) > wire.MaxPayload-len(out.State) {
		return 0, errOutputTooLarge
	}
	var sections [2]wire.SectionView
	n := 0
	if out.StatePresent || len(out.State) != 0 {
		sections[n] = wire.SectionView{Kind: wire.SectionState, Size: len(out.State)}
		n++
	}
	if out.MutationPresent || len(out.Mutation) != 0 {
		sections[n] = wire.SectionView{Kind: wire.SectionMutation, Size: len(out.Mutation)}
		n++
	}
	arena := align8(wire.HeaderLen + n*wire.SectionStride)
	end := arena
	for i := range n {
		sections[i].Off = align4(end)
		end = sections[i].Off + sections[i].Size
	}
	total := align8(end)
	if total > len(dst) {
		return 0, errOutputFull
	}
	clear(dst[:total])
	wire.WriteEnvelopeHeader(dst, magicOut, total, n, arena, end-arena)
	for i, s := range sections[:n] {
		wire.WriteSection(dst, i, s.Kind, s.Off, s.Size, 0)
		payload := out.State
		if s.Kind == wire.SectionMutation {
			payload = out.Mutation
		}
		copy(s.Bytes(dst), payload)
	}
	return total, nil
}

// ParseOutput validates one OUT envelope and returns zero-copy payload views.
// Callers must consume the views before the guest instance is reused.
func ParseOutput(win []byte) (Output, error) {
	table, ok := wire.ReadSectionTable(win, magicOut)
	if !ok {
		return Output{}, errOutputCorrupt
	}
	out := Output{}
	for _, s := range table.Sections[:table.N] {
		if s.Count != 0 || s.Off < table.ArenaOff || s.Off+s.Size > table.ArenaEnd {
			return Output{}, errOutputCorrupt
		}
		switch s.Kind {
		case wire.SectionState:
			out.State = s.Bytes(win)
			out.StatePresent = true
		case wire.SectionMutation:
			out.Mutation = s.Bytes(win)
			out.MutationPresent = true
		default:
			return Output{}, errOutputCorrupt
		}
	}
	return out, nil
}
