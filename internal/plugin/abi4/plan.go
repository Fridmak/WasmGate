// Package abi4 contains the stable building blocks for the WasmGate ABI v4
// event-oriented plugin protocol: the versioned envelope, the inspection plan,
// capability bits and the on_event engine. Proxy integration is deliberately
// separate and lands in M1.
package abi4

import (
	"errors"
	"fmt"
)

// Phase identifies the HTTP pipeline stage carried by one on_event call.
type Phase uint32

const (
	PhaseRequestHeaders  Phase = 1
	PhaseRequestBody     Phase = 2
	PhaseResponseHeaders Phase = 3
	PhaseResponseBody    Phase = 4
	PhaseOpaqueStream    Phase = 5
)

// Flags describes event boundaries and truncation.
type Flags uint32

const (
	FlagFirst Flags = 1 << iota
	FlagLast
	FlagTruncated
)

// Plan is the set of phases a listener wants to inspect. A plan is resolved
// once when a snapshot is built; the data path must not parse mode strings.
type Plan uint8

const (
	InspectRequestHeaders Plan = 1 << iota
	InspectRequestBody
	InspectResponseHeaders
	InspectResponseBody
	InspectOpaqueStream
)

// Has reports whether the plan contains phase.
func (p Plan) Has(phase Phase) bool {
	switch phase {
	case PhaseRequestHeaders:
		return p&InspectRequestHeaders != 0
	case PhaseRequestBody:
		return p&InspectRequestBody != 0
	case PhaseResponseHeaders:
		return p&InspectResponseHeaders != 0
	case PhaseResponseBody:
		return p&InspectResponseBody != 0
	case PhaseOpaqueStream:
		return p&InspectOpaqueStream != 0
	default:
		return false
	}
}

// Validate rejects an empty or contradictory plan.
func (p Plan) Validate() error {
	if p == 0 {
		return errors.New("abi4: inspection plan is empty")
	}
	if p&InspectOpaqueStream != 0 &&
		p&(InspectRequestHeaders|InspectRequestBody|InspectResponseHeaders|InspectResponseBody) != 0 {
		return errors.New("abi4: opaque stream cannot be combined with HTTP phases")
	}
	if p&InspectRequestBody != 0 && p&InspectRequestHeaders == 0 {
		return errors.New("abi4: request body requires request headers")
	}
	if p&InspectResponseBody != 0 && p&InspectResponseHeaders == 0 {
		return errors.New("abi4: response body requires response headers")
	}
	return nil
}

// RequiredCaps returns the guest capabilities needed by the plan. Blob is
// implicit in every event and is required separately by the engine.
func (p Plan) RequiredCaps() Caps {
	var c Caps
	if p&InspectRequestHeaders != 0 || p&InspectResponseHeaders != 0 {
		c |= CapHeaderIndex | CapStartLine
	}
	if p&InspectOpaqueStream != 0 {
		c |= CapOpaque
	}
	if p&(InspectRequestBody|InspectResponseBody) != 0 {
		c |= CapStateIn | CapStateOut
	}
	return c
}

// Caps is the guest capability bitset returned by wg_caps().
type Caps uint64

const (
	CapBlob Caps = 1 << iota
	CapHeaderIndex
	CapStartLine
	CapConnInfo
	CapStateIn
	CapStateOut
	CapOpaque
)

// Has reports whether all bits in need are present.
func (c Caps) Has(need Caps) bool { return c&need == need }

// Verdict is the low 32 bits of the on_event result: 0 allow, 1 deny, >=2 a
// route index into route_pools[v-2].
type Verdict int32

const (
	VerdictAllow Verdict = 0
	VerdictDeny  Verdict = 1
)

// ValidateVerdict rejects unknown values. 0 and 1 are allow/deny; values >=2
// are route indices into route_pools[v-2]. A negative value is a guest bug and
// is rejected instead of being silently treated as deny.
func ValidateVerdict(v Verdict, routePools int) error {
	if v == VerdictAllow || v == VerdictDeny {
		return nil
	}
	if v < 2 {
		return fmt.Errorf("abi4: invalid verdict %d", v)
	}
	idx := int(v) - 2
	if idx < 0 || idx >= routePools {
		return fmt.Errorf("abi4: route verdict %d is outside %d route pools", v, routePools)
	}
	return nil
}
