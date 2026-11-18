package envelope

import (
	"encoding/binary"

	"wasmgate/internal/plugin/abi4/wire"
)

// Parse validates one envelope and returns zero-copy views into win. It rejects
// every out-of-bounds offset, overlapping/duplicate section and malformed
// fixed struct before returning a Parsed value.
func Parse(win []byte) (*Parsed, error) {
	le := binary.LittleEndian
	table, ok := wire.ReadSectionTable(win, magicIn)
	if !ok || table.N == 0 || table.Sections[0].Kind != wire.SectionBlob {
		return nil, errCorrupt
	}

	p := &Parsed{Total: table.Total, SectionIDs: make([]int, 0, table.N)}
	for _, s := range table.Sections[:table.N] {
		p.SectionIDs = append(p.SectionIDs, s.Kind)
		switch s.Kind {
		case wire.SectionBlob:
			if s.Count != 0 || s.Off != table.ArenaOff || s.Off+s.Size > table.ArenaEnd {
				return nil, errCorrupt
			}
			p.Blob = s.Bytes(win)
		case wire.SectionState:
			if s.Count != 0 || s.Off < table.ArenaOff+len(p.Blob) || s.Off+s.Size > table.ArenaEnd {
				return nil, errCorrupt
			}
			p.State = s.Bytes(win)
		case wire.SectionIndex:
			if s.Off < table.ArenaEnd || s.Count == 0 || s.Count > wire.MaxEntries || s.Size != int(s.Count)*entLen {
				return nil, errCorrupt
			}
			p.Index = make([]Ent, int(s.Count))
			for j := range p.Index {
				e := win[s.Off+j*entLen:]
				p.Index[j] = Ent{
					NameOff: le.Uint32(e[0:]), ValOff: le.Uint32(e[4:]),
					NameLen: le.Uint16(e[8:]), ValLen: le.Uint16(e[10:]),
					Aux: le.Uint32(e[12:]),
				}
				if p.Index[j].NameLen == 0 ||
					!withinBlob(p.Index[j].NameOff, uint32(p.Index[j].NameLen), table.ArenaOff, len(p.Blob)) ||
					!withinBlob(p.Index[j].ValOff, uint32(p.Index[j].ValLen), table.ArenaOff, len(p.Blob)) {
					return nil, errCorrupt
				}
			}
		case wire.SectionStartLine:
			if s.Count != 0 || s.Size != startLineLen || s.Off < table.ArenaEnd {
				return nil, errCorrupt
			}
			w := s.Bytes(win)
			s := &StartLine{
				MethodOff: le.Uint32(w[0:]), PathOff: le.Uint32(w[4:]), ProtoOff: le.Uint32(w[8:]),
				MethodLen: le.Uint16(w[12:]), PathLen: le.Uint16(w[14:]), ProtoLen: le.Uint16(w[16:]),
				TargetForm: w[18], Flags: w[19],
			}
			if !validStart(s, table.ArenaOff, len(p.Blob)) {
				return nil, errCorrupt
			}
			p.Start = s
		case wire.SectionConnInfo:
			if s.Count != 0 || s.Size != connInfoLen || s.Off < table.ArenaEnd {
				return nil, errCorrupt
			}
			w := s.Bytes(win)
			c := &ConnInfo{Flags: le.Uint32(w[0:]), Family: w[4]}
			copy(c.RemoteIP[:], w[8:24])
			copy(c.LocalIP[:], w[24:40])
			c.RemotePort, c.LocalPort, c.UpstreamPort = le.Uint16(w[40:]), le.Uint16(w[42:]), le.Uint16(w[44:])
			c.SNIOff, c.SNILen = le.Uint32(w[48:]), le.Uint32(w[52:])
			c.ALPNOff, c.ALPNLen = le.Uint32(w[56:]), le.Uint32(w[60:])
			copy(c.UpstreamIP[:], w[64:80])
			if !withinOptionalBlob(c.SNIOff, c.SNILen, table.ArenaOff, len(p.Blob)) ||
				!withinOptionalBlob(c.ALPNOff, c.ALPNLen, table.ArenaOff, len(p.Blob)) {
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
