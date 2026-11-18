package httpinspect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/observe"
	abi4 "wasmgate/internal/plugin/abi4/engine"
)

var (
	errBodyFraming        = errors.New("proxy: unsupported or invalid HTTP/1 body framing")
	errBodyTooLarge       = errors.New("proxy: body exceeds max_body_bytes")
	errBodyMutation       = errors.New("proxy: body mutation changed chunk length")
	errBodyMutationPolicy = errors.New("proxy: body mutation requires mutation_policy replace")
	errPluginDenied       = errors.New("proxy: payload rejected by wasm on_event hook")
)

// Mutating events must fit a replacement and state into the default 4 KiB
// OUT window. Read-only body events can use the full relay buffer instead.
const (
	httpMutationChunk     = 2 * 1024
	httpStateWithMutation = abi4.DefaultOutCap - httpMutationChunk - 64
)

// Exchange is a single HTTP/1 exchange. Both directions enforce framing
// regardless of which phases the plugin observes. After prefix replay each
// body flow is owned by exactly one relay goroutine.
type Exchange struct {
	engine         *abi4.Engine
	connID         uint64
	headRequest    bool
	request        bodyFlow
	response       bodyFlow
	maxHeader      int
	maxBody        int64
	inspectTimeout time.Duration
	metrics        *observe.Metrics
}

type bodyFlow struct {
	engine  *abi4.Engine // nil means enforce framing without a plugin event
	connID  uint64
	phase   abi4.Phase
	left    int64
	started bool
	replace bool
	state   *abi4.StateSlot
	metrics *observe.Metrics
}

func newBodyFlow(engine *abi4.Engine, connID uint64, phase abi4.Phase, replace bool, metrics *observe.Metrics) bodyFlow {
	b := bodyFlow{connID: connID, phase: phase, replace: replace, metrics: metrics}
	if engine.Plan().Has(phase) {
		b.engine = engine
		if engine.Caps().Has(abi4.CapStateIn | abi4.CapStateOut) {
			capacity := abi4.DefaultOutCap
			if replace {
				capacity = httpStateWithMutation
			}
			// These host-selected constants are within the slot limit.
			b.state, _ = abi4.NewStateSlot(capacity)
		}
	}
	return b
}

func NewExchange(engine *abi4.Engine, connID uint64, pc config.PluginConfig, replace bool, metrics *observe.Metrics) *Exchange {
	maxBody := pc.MaxBodyBytes
	if maxBody == 0 {
		maxBody = config.DefaultMaxBodyBytes
	}
	return &Exchange{
		engine: engine, connID: connID, maxHeader: pc.MaxHeaderBytes, maxBody: maxBody,
		inspectTimeout: pc.InspectTimeout, metrics: metrics,
		request:  newBodyFlow(engine, connID, abi4.PhaseRequestBody, replace, metrics),
		response: newBodyFlow(engine, connID, abi4.PhaseResponseBody, replace, metrics),
	}
}

// contentLength reads framing from the same validated header index used by
// the guest. A missing length and a declared zero length are different.
func contentLength(block []byte, headers []abi4.Ent) (length int64, present bool, err error) {
	for _, ent := range headers {
		name := block[ent.NameOff : ent.NameOff+uint32(ent.NameLen)]
		switch {
		case bytes.EqualFold(name, []byte("transfer-encoding")):
			return 0, false, fmt.Errorf("%w: transfer-encoding is not supported", errBodyFraming)
		case bytes.EqualFold(name, []byte("upgrade")), bytes.EqualFold(name, []byte("expect")):
			return 0, false, fmt.Errorf("%w: upgrade and expect are not supported", errBodyFraming)
		case bytes.EqualFold(name, []byte("content-length")):
			if present {
				return 0, false, fmt.Errorf("%w: duplicate content-length", errBodyFraming)
			}
			v := bytes.Trim(block[ent.ValOff:ent.ValOff+uint32(ent.ValLen)], " \t")
			for _, c := range v {
				if c < '0' || c > '9' {
					return 0, false, fmt.Errorf("%w: invalid content-length", errBodyFraming)
				}
			}
			length, err = strconv.ParseInt(string(v), 10, 64)
			if err != nil {
				return 0, false, fmt.Errorf("%w: invalid content-length", errBodyFraming)
			}
			present = true
		}
	}
	return length, present, nil
}

// PrepareRequest validates and inspects all bytes peeked before the dial.
// Changes to the coalesced body are made directly in the replay buffer.
func (x *Exchange) PrepareRequest(ctx context.Context, head, body []byte) (abi4.Verdict, error) {
	var index [abi4.DefaultHeaderIndexCap]abi4.Ent
	n, line, err := abi4.SplitHeaders(head, index[:])
	if err != nil {
		return abi4.VerdictDeny, err
	}
	method := string(head[:line.MethodLen])
	if method == "CONNECT" {
		return abi4.VerdictDeny, fmt.Errorf("%w: CONNECT is not supported", errBodyFraming)
	}
	x.headRequest = method == "HEAD"
	x.request.left, _, err = contentLength(head, index[:n])
	if err != nil {
		return abi4.VerdictDeny, err
	}
	if x.request.left > x.maxBody {
		return abi4.VerdictDeny, fmt.Errorf("%w: request content-length %d", errBodyTooLarge, x.request.left)
	}
	v, err := x.inspectHeaders(ctx, abi4.PhaseRequestHeaders, head, index[:n], &line, x.request.state)
	if err != nil || v == abi4.VerdictDeny {
		return v, err
	}
	if err := x.request.accept(ctx, body, nil); err != nil {
		return abi4.VerdictDeny, err
	}
	return v, nil
}

func (x *Exchange) PrepareResponse(ctx context.Context, head []byte) error {
	var index [abi4.DefaultHeaderIndexCap]abi4.Ent
	n, line, err := abi4.SplitResponseHeaders(head, index[:])
	if err != nil {
		return err
	}
	length, present, err := contentLength(head, index[:n])
	if err != nil {
		return err
	}
	status, _ := strconv.Atoi(string(head[line.PathOff : line.PathOff+uint32(line.PathLen)]))
	switch {
	case status < 200:
		return fmt.Errorf("%w: informational responses are not supported", errBodyFraming)
	case status == 204 && present, status == 205 && length != 0:
		return fmt.Errorf("%w: unexpected content-length for status %d", errBodyFraming, status)
	case x.headRequest || status == 204 || status == 205 || status == 304:
		// Content-Length on HEAD/304 describes a representation, not a body.
		length = 0
	case !present:
		return fmt.Errorf("%w: response requires content-length", errBodyFraming)
	}
	if length > x.maxBody {
		return fmt.Errorf("%w: response content-length %d", errBodyTooLarge, length)
	}
	x.response.left = length
	if !x.engine.Plan().Has(abi4.PhaseResponseHeaders) {
		return nil
	}
	v, err := x.inspectHeaders(ctx, abi4.PhaseResponseHeaders, head, index[:n], &line, x.response.state)
	if err != nil {
		return err
	}
	if v != abi4.VerdictAllow {
		return errPluginDenied
	}
	return nil
}

func (x *Exchange) inspectHeaders(ctx context.Context, phase abi4.Phase, block []byte, index []abi4.Ent, line *abi4.StartLine, slot *abi4.StateSlot) (abi4.Verdict, error) {
	r, err := x.engine.OnEventResult(ctx, x.connID, phase, abi4.FlagFirst|abi4.FlagLast,
		abi4.Event{Block: block, Index: index, Start: line, State: slot.Bytes()})
	if err != nil {
		x.metrics.PluginDiscard()
		return abi4.VerdictDeny, err
	}
	if r.MutationPresent {
		return abi4.VerdictDeny, errors.New("proxy: header mutations are not supported")
	}
	if r.StatePresent {
		if err := slot.Commit(r.State); err != nil {
			return abi4.VerdictDeny, err
		}
	}
	return r.Verdict, nil
}
