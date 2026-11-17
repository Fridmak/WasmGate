package tlspeek

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestConnFillAppendsAndDrains(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	writeErr := make(chan error, 1)
	go func() {
		if _, err := server.Write([]byte("abc")); err != nil {
			writeErr <- err
			return
		}
		writeErr <- nil
	}()

	_, wrapped, err := PeekChunk(client, time.Second)
	if err != nil {
		t.Fatalf("PeekChunk: %v", err)
	}
	conn, ok := wrapped.(*Conn)
	if !ok {
		t.Fatal("PeekChunk did not return *Conn")
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(conn.Bytes()); got != "abc" {
		t.Fatalf("Buffered = %q, want abc", got)
	}

	go func() {
		if _, err := server.Write([]byte("def")); err != nil {
			writeErr <- err
			return
		}
		writeErr <- nil
	}()
	if n, err := conn.Fill(); err != nil || n != 3 {
		t.Fatalf("Fill = (%d, %v), want (3, nil)", n, err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(conn.Bytes()); got != "abcdef" {
		t.Fatalf("Buffered after Fill = %q, want abcdef", got)
	}

	var out bytes.Buffer
	raw, n, err := conn.DrainTo(&out)
	if err != nil || n != 6 {
		t.Fatalf("DrainTo = (%v, %d, %v)", raw, n, err)
	}
	if out.String() != "abcdef" {
		t.Fatalf("DrainTo wrote %q", out.String())
	}
	if _, err := conn.Fill(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Fill after release = %v, want net.ErrClosed", err)
	}
}

func TestConnFillFullBuffer(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	payload := bytes.Repeat([]byte{'a'}, recordHeaderLen+maxRecordPayload)
	done := make(chan struct{})
	go func() {
		_, _ = server.Write(payload)
		close(done)
	}()
	_, wrapped, err := PeekChunk(client, time.Second)
	if err != nil {
		t.Fatalf("PeekChunk: %v", err)
	}
	conn, ok := wrapped.(*Conn)
	if !ok {
		t.Fatal("PeekChunk did not return *Conn")
	}
	for {
		n, ferr := conn.Fill()
		if ferr != nil {
			t.Fatalf("Fill: %v", ferr)
		}
		if n == 0 {
			break
		}
	}
	if _, err := conn.Fill(); err != nil {
		t.Fatalf("Fill on full buffer: %v", err)
	}
	if len(conn.Bytes()) != len(payload) {
		t.Fatalf("buffered %d, want %d", len(conn.Bytes()), len(payload))
	}
	<-done
}

func TestConnFillEOF(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	go func() { _, _ = server.Write([]byte("a")) }()
	_, wrapped, err := PeekChunk(client, time.Second)
	if err != nil {
		t.Fatalf("PeekChunk: %v", err)
	}
	conn, ok := wrapped.(*Conn)
	if !ok {
		t.Fatal("PeekChunk did not return *Conn")
	}
	_ = server.Close()
	if _, err := conn.Fill(); !errors.Is(err, io.EOF) {
		t.Fatalf("Fill after peer close = %v, want io.EOF", err)
	}
}
