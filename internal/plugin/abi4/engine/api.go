package engine

import (
	"wasmgate/internal/plugin/abi4/envelope"
	"wasmgate/internal/plugin/abi4/output"
	"wasmgate/internal/plugin/abi4/plan"
	"wasmgate/internal/plugin/abi4/state"
)

// The engine package is the convenient runtime entry point. Wire-level types
// remain implemented in focused subpackages, while these aliases keep normal
// engine call sites from having to import every ABI layer separately.
type (
	Phase     = plan.Phase
	Flags     = plan.Flags
	Plan      = plan.Plan
	Caps      = plan.Caps
	Verdict   = plan.Verdict
	Event     = envelope.Event
	Ent       = envelope.Ent
	StartLine = envelope.StartLine
	ConnInfo  = envelope.ConnInfo
	Parsed    = envelope.Parsed
	Layout    = envelope.Layout
	Output    = output.Output
	StateSlot = state.Slot
)

const (
	PhaseRequestHeaders  = plan.PhaseRequestHeaders
	PhaseRequestBody     = plan.PhaseRequestBody
	PhaseResponseHeaders = plan.PhaseResponseHeaders
	PhaseResponseBody    = plan.PhaseResponseBody
	PhaseOpaqueStream    = plan.PhaseOpaqueStream

	FlagFirst          = plan.FlagFirst
	FlagLast           = plan.FlagLast
	FlagTruncated      = plan.FlagTruncated
	FlagClientToServer = plan.FlagClientToServer
	FlagServerToClient = plan.FlagServerToClient

	InspectRequestHeaders  = plan.InspectRequestHeaders
	InspectRequestBody     = plan.InspectRequestBody
	InspectResponseHeaders = plan.InspectResponseHeaders
	InspectResponseBody    = plan.InspectResponseBody
	InspectOpaqueStream    = plan.InspectOpaqueStream
	InspectLifecycle       = plan.InspectLifecycle

	CapBlob        = plan.CapBlob
	CapHeaderIndex = plan.CapHeaderIndex
	CapStartLine   = plan.CapStartLine
	CapConnInfo    = plan.CapConnInfo
	CapStateIn     = plan.CapStateIn
	CapStateOut    = plan.CapStateOut
	CapOpaque      = plan.CapOpaque
	CapMutation    = plan.CapMutation
	CapLifecycle   = plan.CapLifecycle

	VerdictAllow = plan.VerdictAllow
	VerdictDeny  = plan.VerdictDeny

	MaxStateSlot = state.MaxStateSlot
)

func Write(dst []byte, ev Event) (int, error) { return envelope.Write(dst, ev) }

func Parse(win []byte) (*Parsed, error) { return envelope.Parse(win) }

func SplitHeaders(block []byte, idx []Ent) (int, StartLine, error) {
	return envelope.SplitHeaders(block, idx)
}

func SplitResponseHeaders(block []byte, idx []Ent) (int, StartLine, error) {
	return envelope.SplitResponseHeaders(block, idx)
}

func WriteOutput(dst []byte, out Output) (int, error) { return output.WriteOutput(dst, out) }

func ParseOutput(win []byte) (Output, error) { return output.ParseOutput(win) }

func NewStateSlot(capacity int) (*StateSlot, error) { return state.NewStateSlot(capacity) }

func ValidateVerdict(v Verdict, routePools int) error { return plan.ValidateVerdict(v, routePools) }
