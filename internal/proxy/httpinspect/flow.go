package httpinspect

import (
	"context"
	"errors"
	"fmt"
	"io"

	abi4 "wasmgate/internal/plugin/abi4/engine"
	"wasmgate/internal/proxy/relay"
)

// accept checks the whole read before forwarding any of it. It never clips
// a read that crosses the declared boundary. A short transport delivers a
// terminal truncation notification but neither its bytes nor its OUT effects.
func (b *bodyFlow) accept(ctx context.Context, data []byte, readErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if int64(len(data)) > b.left {
		return fmt.Errorf("%w: bytes beyond content-length", errBodyFraming)
	}
	truncated := readErr != nil && int64(len(data)) < b.left
	if b.engine != nil {
		maxChunk := relay.IdleBufferSize
		if b.replace {
			maxChunk = httpMutationChunk
		}
		for rest := data; len(rest) != 0 || truncated; {
			n := min(len(rest), maxChunk)
			end := n == len(rest)
			if err := b.inspect(ctx, rest[:n], end && truncated); err != nil {
				return err
			}
			if end {
				break
			}
			rest = rest[n:]
		}
	} else {
		b.left -= int64(len(data))
	}
	if truncated {
		if errors.Is(readErr, io.EOF) {
			readErr = io.ErrUnexpectedEOF
		}
		return fmt.Errorf("%w: incomplete body: %w", errBodyFraming, readErr)
	}
	return nil
}

func (b *bodyFlow) inspect(ctx context.Context, chunk []byte, truncated bool) error {
	var flags abi4.Flags
	if !b.started {
		flags |= abi4.FlagFirst
	}
	if int64(len(chunk)) == b.left || truncated {
		flags |= abi4.FlagLast
	}
	if truncated {
		flags |= abi4.FlagTruncated
	}
	r, err := b.engine.OnEventResult(ctx, b.connID, b.phase, flags, abi4.Event{Block: chunk, State: b.state.Bytes()})
	if err != nil {
		b.metrics.PluginDiscard()
		return err
	}
	if r.Verdict != abi4.VerdictAllow {
		return errPluginDenied
	}
	if truncated {
		return nil
	}
	if r.MutationPresent {
		if !b.replace {
			return errBodyMutationPolicy
		}
		if len(r.Mutation) != len(chunk) {
			return errBodyMutation
		}
	}
	if r.StatePresent {
		if err := b.state.Commit(r.State); err != nil {
			return err
		}
	}
	if r.MutationPresent {
		copy(chunk, r.Mutation)
	}
	b.left -= int64(len(chunk))
	b.started = true
	return nil
}
