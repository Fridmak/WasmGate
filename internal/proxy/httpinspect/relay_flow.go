package httpinspect

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

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
