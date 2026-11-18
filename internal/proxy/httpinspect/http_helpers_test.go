package httpinspect

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"wasmgate/internal/tlspeek"
)

func TestReadHeaderBlockCollectsSplitHead(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	go func() {
		_, _ = peer.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n"))
		time.Sleep(10 * time.Millisecond)
		_, _ = peer.Write([]byte("X-Test: 1\r\n\r\nbody"))
	}()

	_, wrapped, err := tlspeek.PeekChunk(client, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pc, ok := wrapped.(*tlspeek.Conn)
	if !ok {
		t.Fatal("PeekChunk did not return a buffered connection")
	}
	prefix, headers, err := ReadHeaderBlock(pc, 1024, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(headers) != "GET / HTTP/1.1\r\nHost: x\r\nX-Test: 1\r\n\r\n" {
		t.Fatalf("headers = %q", headers)
	}
	if string(prefix) != string(headers)+"body" {
		t.Fatalf("prefix = %q", prefix)
	}
}

func TestReadHeaderBlockRejectsOversizedAndHTTP2(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write string
	}{
		{"oversized", "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"},
		{"http2 preface", "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			go func() { _, _ = peer.Write([]byte(tc.write)) }()
			_, wrapped, err := tlspeek.PeekChunk(client, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			pc := wrapped.(*tlspeek.Conn)
			if _, _, err := ReadHeaderBlock(pc, 8, time.Second); err == nil {
				t.Fatal("invalid header block accepted")
			}
		})
	}
}

func TestWatchHTTPIdleExpiresAndTouchExtends(t *testing.T) {
	expired := make(chan struct{}, 1)
	touch, stop := watchHTTPIdle(30*time.Millisecond, func() { expired <- struct{}{} })
	defer stop()
	touch()
	select {
	case <-expired:
		t.Fatal("idle watchdog fired immediately after touch")
	case <-time.After(10 * time.Millisecond):
	}
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("idle watchdog did not fire")
	}
}

func TestExchangeJoinRelaysResponse(t *testing.T) {
	x := &Exchange{
		engine:         httpTestEngine(t, v4Verdict(0), bodyTestPlan),
		maxHeader:      256,
		inspectTimeout: time.Second,
		request:        bodyFlow{left: 0},
		response:       bodyFlow{left: 0},
	}
	server := &scriptedHTTPConn{reads: []string{"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"}}
	client := &scriptedHTTPConn{}
	rx, tx := x.Join(context.Background(), client, server, 0)
	const response = "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
	if tx != 0 || rx != int64(len(response)) || client.wrote.String() != response {
		t.Fatalf("join = (%d, %d, %q)", rx, tx, client.wrote.String())
	}
}

func TestWriteHTTPDetectsShortWrite(t *testing.T) {
	short := shortWriter{}
	if _, err := writeHTTP(short, []byte("data"), func() {}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
