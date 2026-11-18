package envelope

import (
	"errors"
	"testing"
)

func TestSplitResponseHeadersRoundTrip(t *testing.T) {
	block := []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\nX-Test: value\r\n\r\n")
	idx := make([]Ent, 4)
	n, line, err := SplitResponseHeaders(block, idx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("header count = %d, want 2", n)
	}
	if line.ProtoOff != 0 || line.ProtoLen != 8 || line.PathOff != 9 || line.PathLen != 3 ||
		line.MethodOff == 0 || line.MethodLen != 2 {
		t.Fatalf("response start line = %+v", line)
	}

	env := mustWrite(t, Event{Block: block, Index: idx[:n], Start: &line})
	parsed, err := Parse(env)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := parsed.Header(env, "content-length"); !ok || string(got) != "5" {
		t.Fatalf("content-length = %q, %v", got, ok)
	}
}

func TestSplitResponseHeadersAcceptsHTTPVersionsAndStatuses(t *testing.T) {
	for _, line := range []string{
		"HTTP/1.0 100 Continue",
		"HTTP/1.1 204",
		"HTTP/1.1 404 Not Found",
		"HTTP/1.1 500 Internal Server Error",
	} {
		t.Run(line, func(t *testing.T) {
			n, _, err := SplitResponseHeaders([]byte(line+"\r\n\r\n"), make([]Ent, 2))
			if err != nil || n != 0 {
				t.Fatalf("split = (%d, %v)", n, err)
			}
		})
	}
}

func TestSplitResponseHeadersRejectsMalformed(t *testing.T) {
	for _, line := range []string{
		"HTTP/2.0 200 OK",
		"HTTP/1.1 000 Invalid",
		"HTTP/1.1 600 Invalid",
		"HTTP/1.1 20 Invalid",
		"HTTP/1.1 200Invalid",
		"HTTP/1.1 200\x00Bad",
		"HTTP/1.1 200 Bad\rBad",
	} {
		t.Run(line, func(t *testing.T) {
			if _, _, err := SplitResponseHeaders([]byte(line+"\r\n\r\n"), make([]Ent, 4)); err == nil {
				t.Fatalf("accepted malformed status line %q", line)
			}
		})
	}

	for _, block := range [][]byte{
		[]byte("HTTP/1.1 200 OK\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\nX: y\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\nX: y\r\n\r\nextra"),
	} {
		if _, _, err := SplitResponseHeaders(block, make([]Ent, 4)); err == nil {
			t.Fatalf("accepted malformed response block %q", block)
		}
	}
}

func TestSplitResponseHeadersRejectsTooManyHeaders(t *testing.T) {
	block := []byte("HTTP/1.1 200 OK\r\nA: b\r\nB: c\r\n\r\n")
	_, _, err := SplitResponseHeaders(block, make([]Ent, 1))
	if !errors.Is(err, errTooManyHeaders) {
		t.Fatalf("err = %v, want %v", err, errTooManyHeaders)
	}
}
