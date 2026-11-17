package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"wasmgate/internal/tlspeek"
)

// HTTP/1 header terminator and the HTTP/2 connection preface. The preface is
// checked before the terminator because it contains CRLFCRLF itself.
var (
	headerTerminator = []byte("\r\n\r\n")
	http2Preface     = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
)

var (
	errHeaderIncomplete = errors.New("proxy: incomplete HTTP request header block")
	errHeaderTooLarge   = errors.New("proxy: HTTP request header block exceeds the configured limit")
	errHTTP2Preface     = errors.New("proxy: HTTP/2 preface on a headers-mode listener")
)

// readHeaderBlock reads one complete HTTP/1 request header block from a peeked
// connection. It returns the full prefix (headers plus any body bytes already
// read) and the header slice, including the final CRLFCRLF.
//
// The deadline is absolute for the whole block, not per Read: a slow client
// cannot extend the inspection window by trickling bytes. The caller owns the
// connection and must replay prefix before joining the two sides.
func readHeaderBlock(c *tlspeek.Conn, maxBytes int, timeout time.Duration) (prefix, headers []byte, err error) {
	if c == nil {
		return nil, nil, fmt.Errorf("%w: no connection", errHeaderIncomplete)
	}
	if maxBytes <= 0 {
		return nil, nil, fmt.Errorf("%w: invalid limit %d", errHeaderTooLarge, maxBytes)
	}
	if timeout <= 0 {
		return nil, nil, fmt.Errorf("%w: invalid timeout %s", errHeaderIncomplete, timeout)
	}
	if err := c.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, nil, fmt.Errorf("%w: set deadline: %v", errHeaderIncomplete, err)
	}
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()

	prefix = c.Bytes()
	for {
		// A prefix that is still a prefix of the HTTP/2 preface is ambiguous:
		// keep reading rather than parsing it as HTTP/1. Only the exact full
		// preface is rejected as HTTP/2.
		ambiguous := len(prefix) < len(http2Preface) && bytes.HasPrefix(http2Preface, prefix)
		if !ambiguous && bytes.HasPrefix(prefix, http2Preface) {
			return nil, nil, errHTTP2Preface
		}
		if !ambiguous {
			if idx := bytes.Index(prefix, headerTerminator); idx >= 0 {
				end := idx + len(headerTerminator)
				if end > maxBytes {
					return nil, nil, fmt.Errorf("%w: %d bytes", errHeaderTooLarge, end)
				}
				return prefix, prefix[:end], nil
			}
		}
		if len(prefix) >= maxBytes {
			return nil, nil, fmt.Errorf("%w: %d bytes", errHeaderTooLarge, len(prefix))
		}

		n, readErr := c.Fill()
		if n > 0 {
			// A Read may legally return data together with io.EOF or a timeout.
			// Process the appended bytes first; the next Fill will surface the
			// terminal error if the block is still incomplete.
			prefix = c.Bytes()
			continue
		}
		if readErr != nil {
			return nil, nil, fmt.Errorf("%w: %v", errHeaderIncomplete, readErr)
		}
		if n == 0 {
			// Fill returns (0, nil) only when the peek buffer is full.
			return nil, nil, fmt.Errorf("%w: %d bytes", errHeaderTooLarge, len(prefix))
		}
	}
}
