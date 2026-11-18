package engine

import (
	"context"
	"fmt"
)

// OnRequestHeaders is the typed request-header entry point. It validates and
// indexes the complete request head before dispatching the event.
// The header index is pooled and bounded by Options.HeaderIndexCap;
// SplitHeaders rejects overlong or malformed requests instead of silently
// truncating the index.
func (e *Engine) OnRequestHeaders(ctx context.Context, connID uint64, block []byte) (Verdict, error) {
	if e == nil {
		return VerdictAllow, nil
	}
	buf, _ := e.idxPool.Get().(*[]Ent)
	if buf == nil {
		b := make([]Ent, e.headerCap)
		buf = &b
	}
	defer e.idxPool.Put(buf)
	n, start, err := SplitHeaders(block, *buf)
	if err != nil {
		return VerdictDeny, err
	}
	return e.OnEvent(ctx, connID, PhaseRequestHeaders, FlagFirst|FlagLast, Event{
		Block: block, Index: (*buf)[:n], Start: &start,
	})
}

// OnOpaqueFirst is the pre-dial opaque first client-to-server chunk entry point.
func (e *Engine) OnOpaqueFirst(ctx context.Context, connID uint64, chunk []byte) (Verdict, error) {
	return e.OnEvent(ctx, connID, PhaseOpaqueStream, FlagFirst|FlagClientToServer, Event{Block: chunk})
}

// OnOpaqueChunk is the direction-aware opaque entry point. dir 0 is
// client-to-server, dir 1 is server-to-client. The direction is encoded into
// Flags; an event carrying both directions is rejected.
func (e *Engine) OnOpaqueChunk(ctx context.Context, connID uint64, dir int32, flags Flags, chunk []byte) (Verdict, error) {
	switch dir {
	case 0:
		flags |= FlagClientToServer
	case 1:
		flags |= FlagServerToClient
	default:
		return VerdictDeny, fmt.Errorf("abi4: invalid opaque direction %d", dir)
	}
	return e.OnEvent(ctx, connID, PhaseOpaqueStream, flags, Event{Block: chunk})
}

// OnStream is the post-dial client-to-server opaque chunk entry point. Use
// OnOpaqueChunk when the host needs direction-aware stream inspection.
func (e *Engine) OnStream(ctx context.Context, connID uint64, chunk []byte) (Verdict, error) {
	return e.OnOpaqueChunk(ctx, connID, 0, 0, chunk)
}
