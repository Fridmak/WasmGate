package engine

import (
	"context"
	"errors"
)

func (e *Engine) discard(inst *instance) {
	e.discarded.Add(1)
	_ = inst.mod.Close(context.Background())
	<-e.sem
}

// borrow reserves one of the engine's concurrent instance slots, then reuses a
// pooled instance or instantiates a new one. The slot is released by release or
// discard. Waiting is bounded by the caller's context and the load deadline.
func (e *Engine) borrow(ctx context.Context) (*instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	loadCtx, cancel := context.WithTimeout(ctx, e.loadTimeout)
	defer cancel()
	select {
	case e.sem <- struct{}{}:
	case <-loadCtx.Done():
		return nil, loadCtx.Err()
	}
	select {
	case inst := <-e.pool:
		return inst, nil
	default:
	}
	inst, err := e.instantiate(loadCtx)
	if err != nil {
		<-e.sem
		return nil, err
	}
	if inst.abi != e.abi || inst.caps != e.caps {
		e.discard(inst)
		return nil, errors.New("abi4: inconsistent module ABI or capabilities")
	}
	return inst, nil
}

func (e *Engine) release(inst *instance) {
	select {
	case e.pool <- inst:
	default:
		_ = inst.mod.Close(context.Background())
	}
	<-e.sem
}

// ABI returns the validated ABI version reported by the guest.
func (e *Engine) ABI() uint32 { return e.abi }

// Caps returns the validated guest capability bitset.
func (e *Engine) Caps() Caps { return e.caps }

// Plan returns the immutable phase plan used by the engine.
func (e *Engine) Plan() Plan { return e.plan }

// Discarded returns the number of guest instances destroyed after a trap,
// timeout, invalid output, or inconsistent instance metadata.
func (e *Engine) Discarded() int64 { return e.discarded.Load() }

// Retain takes a shared reference to the engine. Every Retain must be paired
// with one Release. The engine started with a single reference owned by its
// creator.
func (e *Engine) Retain() {
	if e == nil {
		return
	}
	e.refs.Add(1)
}

// Release drops a shared reference and closes the engine when the last one is
// gone. It is safe to call after Close: the runtime close is idempotent.
func (e *Engine) Release() {
	if e == nil {
		return
	}
	if e.refs.Add(-1) == 0 {
		_ = e.Close(context.Background())
	}
}

// Close stops the runtime. Idempotent. Callers that share an engine through
// Retain/Release should use Release instead of closing a shared reference.
func (e *Engine) Close(_ context.Context) error {
	if e == nil {
		return nil
	}
	e.closeOnce.Do(func() {
		_ = e.rt.Close(context.Background())
		for len(e.pool) > 0 {
			<-e.pool
		}
	})
	return nil
}
