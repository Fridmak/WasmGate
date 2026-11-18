package envelope

import (
	"bytes"
	"errors"

	"wasmgate/internal/plugin/abi4/wire"
)

const maxU16 = 1<<16 - 1

var (
	errMalformedHeader = errors.New("envelope: malformed header block")
	errTooManyHeaders  = errors.New("envelope: too many headers")
)

// ErrTooManyHeaders identifies a header index that exceeded its configured
// capacity. It is exported so higher-level engine tests and callers can
// distinguish this fail-closed condition from generic malformed input.
var ErrTooManyHeaders = errTooManyHeaders

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
	if len(block) > wire.MaxPayload {
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
			NameOff: uint32(start), NameLen: uint16(nameLen), // #nosec G115 -- block length is wire.MaxPayload and lengths fit wire fields
			ValOff: uint32(start + vs), ValLen: uint16(valLen), // #nosec G115 -- block length is wire.MaxPayload and lengths fit wire fields
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
