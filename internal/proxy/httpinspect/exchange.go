package httpinspect

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"wasmgate/internal/proxy/relay"
)

// join relays only the bytes of the first exchange and closes both sockets
// once the final response is relayed. The two directions run independently
// so an early final response can terminate an unfinished upload.
func (x *Exchange) Join(ctx context.Context, client, server net.Conn, idle time.Duration) (rx, tx int64) {
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
			buf := relay.GetBuffer()
			defer relay.PutBuffer(buf)
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

func (x *Exchange) relayResponse(ctx context.Context, client, server net.Conn, touch func()) int64 {
	buf := relay.GetBuffer()
	defer relay.PutBuffer(buf)
	prefix, err := readResponsePrefix(server, *buf, x.maxHeader, x.inspectTimeout, touch)
	if err != nil {
		x.metrics.HeaderIncomplete()
		return 0
	}
	if err := x.PrepareResponse(ctx, prefix.data[:prefix.headEnd]); err != nil {
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
