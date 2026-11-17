package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/observe"
	"wasmgate/internal/plugin/abi4"
)

var (
	errBodyFraming        = errors.New("proxy: unsupported or invalid HTTP/1 body framing")
	errBodyMutation       = errors.New("proxy: body mutation changed chunk length")
	errBodyMutationPolicy = errors.New("proxy: body mutation requires mutation_policy replace")
)

// Mutating events must fit a replacement and state into the default 4 KiB
// OUT window. Read-only body events can use the full relay buffer instead.
const (
	httpMutationChunk     = 2 * 1024
	httpStateWithMutation = abi4.DefaultOutCap - httpMutationChunk - 64
)

// httpExchange is a single HTTP/1 exchange. Both directions enforce framing
// regardless of which phases the plugin observes. After prefix replay each
// body flow is owned by exactly one relay goroutine.
type httpExchange struct {
	engine         *abi4.Engine
	connID         uint64
	headRequest    bool
	request        bodyFlow
	response       bodyFlow
	maxHeader      int
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

func newHTTPExchange(engine *abi4.Engine, connID uint64, pc config.PluginConfig, replace bool, metrics *observe.Metrics) *httpExchange {
	return &httpExchange{
		engine: engine, connID: connID, maxHeader: pc.MaxHeaderBytes, inspectTimeout: pc.InspectTimeout, metrics: metrics,
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

// prepareRequest validates and inspects all bytes peeked before the dial.
// Changes to the coalesced body are made directly in the replay buffer.
func (x *httpExchange) prepareRequest(ctx context.Context, head, body []byte) (abi4.Verdict, error) {
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
	v, err := x.inspectHeaders(ctx, abi4.PhaseRequestHeaders, head, index[:n], &line, x.request.state)
	if err != nil || v == abi4.VerdictDeny {
		return v, err
	}
	if err := x.request.accept(ctx, body, nil); err != nil {
		return abi4.VerdictDeny, err
	}
	return v, nil
}

func (x *httpExchange) prepareResponse(ctx context.Context, head []byte) error {
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

func (x *httpExchange) inspectHeaders(ctx context.Context, phase abi4.Phase, block []byte, index []abi4.Ent, line *abi4.StartLine, slot *abi4.StateSlot) (abi4.Verdict, error) {
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
		maxChunk := idleBufferSize
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

// join relays only the bytes of the first exchange and closes both sockets
// once the final response is relayed. The two directions run independently
// so an early final response can terminate an unfinished upload.
func (x *httpExchange) join(ctx context.Context, client, server net.Conn, idle time.Duration) (rx, tx int64) {
	ctx, cancel := context.WithCancel(ctx)
	closeBoth := sync.OnceFunc(func() {
		cancel()
		_ = server.Close()
		_ = client.Close()
	})
	stopCancel := context.AfterFunc(ctx, closeBoth)
	defer stopCancel()
	touch, stopIdle := watchHTTPIdle(idle, closeBoth)
	defer stopIdle()
	var req sync.WaitGroup
	req.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("proxy HTTP request relay panicked", "panic", r, "stack", string(debug.Stack()))
				closeBoth()
			}
		}()
		var err error
		if x.request.left != 0 {
			buf := bufferPool.Get().(*[]byte)
			defer bufferPool.Put(buf)
			tx, err = x.request.relay(ctx, server, client, *buf, touch)
		}
		if err != nil {
			closeBoth()
		}
	})
	// A complete request needs no FIN. Keep the upstream socket open until
	// the response finishes, rather than cancelling the backend's handler.
	defer func() {
		closeBoth()
		req.Wait()
	}()
	rx = x.relayResponse(ctx, client, server, touch)
	return
}

func (x *httpExchange) relayResponse(ctx context.Context, client, server net.Conn, touch func()) int64 {
	buf := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(buf)
	prefix, err := readResponsePrefix(server, *buf, x.maxHeader, x.inspectTimeout, touch)
	if err != nil {
		x.metrics.HeaderIncomplete()
		return 0
	}
	if err := x.prepareResponse(ctx, prefix.data[:prefix.headEnd]); err != nil {
		x.metrics.RejectPlugin()
		return 0
	}
	if err := x.response.accept(ctx, prefix.data[prefix.headEnd:], prefix.readErr); err != nil {
		x.metrics.RejectPlugin()
		return 0
	}
	// Do not let split headers, or a body coalesced with headers, escape before
	// the phase verdicts. Write exactly the buffer that was inspected/mutated.
	total, err := writeHTTP(client, prefix.data, touch)
	if err != nil || x.response.left == 0 {
		return total
	}
	n, _ := x.response.relay(ctx, client, server, *buf, touch)
	return total + n
}

type responsePrefix struct {
	data    []byte
	headEnd int
	readErr error // a Read can return data and a terminal error together
}

func readResponsePrefix(src net.Conn, buf []byte, limit int, timeout time.Duration, touch func()) (responsePrefix, error) {
	var used int
	for {
		n, err := src.Read(buf[used:])
		if n > 0 {
			touch()
			start := max(0, used-len(headerTerminator)+1)
			if j := bytes.Index(buf[start:used+n], headerTerminator); j >= 0 {
				end := start + j + len(headerTerminator)
				if end > limit {
					return responsePrefix{}, errHeaderTooLarge
				}
				return responsePrefix{buf[:used+n], end, err}, nil
			}
		}
		if err != nil {
			return responsePrefix{}, err
		}
		if used+n >= limit || used+n == len(buf) {
			return responsePrefix{}, errHeaderTooLarge
		}
		// Bound a split header from its first byte, not from the beginning of
		// the upload (a backend can wait for the entire request before replying).
		if used == 0 && n > 0 {
			if err := src.SetReadDeadline(time.Now().Add(timeout)); err != nil {
				return responsePrefix{}, err
			}
			defer func() { _ = src.SetReadDeadline(time.Time{}) }()
		}
		used += n
	}
}

func (b *bodyFlow) relay(ctx context.Context, dst, src net.Conn, buf []byte, touch func()) (int64, error) {
	var total int64
	for b.left > 0 {
		n, rerr := src.Read(buf)
		if n > 0 {
			touch()
		}
		if err := b.accept(ctx, buf[:n], rerr); err != nil {
			if ctx.Err() == nil {
				b.metrics.RejectPlugin()
			}
			return total, err
		}
		if n > 0 {
			w, err := writeHTTP(dst, buf[:n], touch)
			total += w
			if err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

func writeHTTP(dst io.Writer, data []byte, touch func()) (int64, error) {
	n, err := dst.Write(data)
	if n > 0 {
		touch()
	}
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}

// Unlike the opaque relay's read deadlines, this watchdog cannot overwrite
// the absolute deadline of a split response head. Either direction's progress
// keeps the connection alive, and a stalled Write is covered too.
func watchHTTPIdle(idle time.Duration, expire func()) (touch, stop func()) {
	if idle <= 0 {
		return func() {}, func() {}
	}
	var mu sync.Mutex
	var timer *time.Timer
	deadline := time.Now().Add(idle)
	stopped := false
	mu.Lock()
	timer = time.AfterFunc(idle, func() {
		mu.Lock()
		if stopped {
			mu.Unlock()
			return
		}
		if left := time.Until(deadline); left > 0 {
			timer.Reset(left)
			mu.Unlock()
			return
		}
		mu.Unlock()
		expire()
	})
	mu.Unlock()
	return func() {
			mu.Lock()
			deadline = time.Now().Add(idle)
			mu.Unlock()
		}, func() {
			mu.Lock()
			stopped = true
			timer.Stop()
			mu.Unlock()
		}
}
