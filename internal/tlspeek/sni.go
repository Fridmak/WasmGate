// Package tlspeek extracts the TLS Server Name Indication (SNI) from the
// initial ClientHello record without terminating TLS, and replays the peeked
// bytes transparently to the upstream connection.
package tlspeek

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	recordHeaderLen = 5
	handshakeRecord = 0x16
	msgClientHello  = 0x01
	extServerName   = 0x0000
	nameTypeHost    = 0x00

	// maxRecordPayload bounds how much of a single TLS record we buffer while
	// peeking ClientHello (16 KiB is the RFC 8446 §5.1 maximum record size).
	maxRecordPayload = 16 * 1024
)

var peekPool = sync.Pool{
	New: func() any {
		b := make([]byte, recordHeaderLen+maxRecordPayload)
		return &b
	},
}

// Conn wraps a net.Conn whose initial bytes were read during an SNI peek.
// Once DrainPrefix has been written to the destination, Underlying returns the
// raw *net.TCPConn so Linux io.Copy can still use splice(2) for the rest of the
// session.
type Conn struct {
	net.Conn
	buf  *[]byte
	data []byte
}

// Bytes returns the unread peeked prefix currently held in the buffer.
func (c *Conn) Bytes() []byte {
	return c.data
}

// Read serves any remaining peeked bytes before reading from the underlying conn.
func (c *Conn) Read(p []byte) (int, error) {
	if len(c.data) > 0 {
		n := copy(p, c.data)
		c.data = c.data[n:]
		if len(c.data) == 0 {
			c.releaseBuf()
		}
		return n, nil
	}
	return c.Conn.Read(p)
}

// DrainTo writes any remaining peeked prefix to w, releases the pooled buffer,
// and returns the underlying net.Conn so the caller can hand the raw socket to
// io.Copy / splice(2).
func (c *Conn) DrainTo(w io.Writer) (net.Conn, int64, error) {
	var written int64
	if len(c.data) > 0 {
		n, err := w.Write(c.data)
		written = int64(n)
		c.data = nil
		c.releaseBuf()
		if err != nil {
			return c.Conn, written, err
		}
	}
	return c.Conn, written, nil
}

// Close releases the pooled buffer (if still held) and closes the underlying conn.
func (c *Conn) Close() error {
	c.data = nil
	c.releaseBuf()
	return c.Conn.Close()
}

func (c *Conn) releaseBuf() {
	if c.buf != nil {
		peekPool.Put(c.buf)
		c.buf = nil
	}
}

// Peek reads the first TLS record (or non-TLS prefix) from conn within timeout,
// extracts the SNI hostname if the record is a TLS ClientHello, and returns a
// wrapped net.Conn that replays all read bytes. If the client sends non-TLS
// traffic, sni is "" and err is nil so the caller can still route to the
// listener's default pool.
func Peek(conn net.Conn, timeout time.Duration) (sni string, wrapped net.Conn, err error) {
	if timeout > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return "", conn, err
		}
		defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	}

	bufp := peekPool.Get().(*[]byte)
	buf := *bufp

	n, err := io.ReadAtLeast(conn, buf[:recordHeaderLen], 1)
	if err != nil {
		peekPool.Put(bufp)
		return "", conn, err
	}

	// If we got a full 5-byte TLS Handshake record header, read the rest of the
	// record payload (up to maxRecordPayload) so the entire ClientHello is in buf.
	if n == recordHeaderLen && buf[0] == handshakeRecord && buf[1] == 0x03 {
		recLen := int(binary.BigEndian.Uint16(buf[3:5]))
		if recLen > 0 && recLen <= maxRecordPayload {
			m, rerr := io.ReadFull(conn, buf[recordHeaderLen:recordHeaderLen+recLen])
			n += m
			if rerr != nil {
				peekPool.Put(bufp)
				return "", conn, rerr
			}
			if raw, ok := ParseClientHelloSNI(buf[:n]); ok {
				sni = strings.ToLower(string(raw))
			}
		}
	}

	return sni, &Conn{Conn: conn, buf: bufp, data: buf[:n]}, nil
}

// PeekChunk reads the first arriving chunk from conn into a pooled buffer (or
// reuses the buffer if conn is already a *Conn from an earlier TLS SNI peek)
// so an L7 WASM plugin can inspect the request payload before dialing the
// upstream, and then replay the chunk via DrainTo.
func PeekChunk(conn net.Conn, timeout time.Duration) (chunk []byte, wrapped net.Conn, err error) {
	if pc, ok := conn.(*Conn); ok {
		return pc.Bytes(), pc, nil
	}
	if timeout > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, conn, err
		}
		defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	}

	bufp := peekPool.Get().(*[]byte)
	buf := *bufp
	n, err := conn.Read(buf)
	if n <= 0 {
		peekPool.Put(bufp)
		return nil, conn, err
	}
	pc := &Conn{Conn: conn, buf: bufp, data: buf[:n]}
	return pc.Bytes(), pc, nil
}

// ParseClientHelloSNI parses a single TLS Handshake record containing a
// ClientHello and returns the raw server_name host_name slice inside rec.
// It performs zero heap allocations (0 B/op, 0 allocs/op).
func ParseClientHelloSNI(rec []byte) (host []byte, ok bool) {
	if len(rec) < recordHeaderLen || rec[0] != handshakeRecord || rec[1] != 0x03 {
		return nil, false
	}
	recLen := int(binary.BigEndian.Uint16(rec[3:5]))
	body := rec[recordHeaderLen:]
	if len(body) < recLen {
		return nil, false
	}
	body = body[:recLen]

	// Handshake header: 1 byte type (ClientHello = 0x01) + 3 bytes length.
	if len(body) < 4 || body[0] != msgClientHello {
		return nil, false
	}
	chLen := int(body[1])<<16 | int(body[2])<<8 | int(body[3])
	ch := body[4:]
	if len(ch) < chLen {
		return nil, false
	}
	ch = ch[:chLen]

	// ClientVersion (2) + Random (32) = 34 bytes, then SessionID length (1).
	if len(ch) < 35 {
		return nil, false
	}
	sidLen := int(ch[34])
	ch = ch[35:]
	if len(ch) < sidLen+2 {
		return nil, false
	}
	ch = ch[sidLen:]

	// CipherSuites (2-byte length).
	csLen := int(binary.BigEndian.Uint16(ch[:2]))
	ch = ch[2:]
	if len(ch) < csLen+1 {
		return nil, false
	}
	ch = ch[csLen:]

	// CompressionMethods (1-byte length).
	compLen := int(ch[0])
	ch = ch[1:]
	if len(ch) < compLen+2 {
		return nil, false
	}
	ch = ch[compLen:]

	// Extensions (2-byte length).
	extTotal := int(binary.BigEndian.Uint16(ch[:2]))
	exts := ch[2:]
	if len(exts) < extTotal {
		return nil, false
	}
	exts = exts[:extTotal]

	for len(exts) >= 4 {
		extType := binary.BigEndian.Uint16(exts[:2])
		extLen := int(binary.BigEndian.Uint16(exts[2:4]))
		exts = exts[4:]
		if len(exts) < extLen {
			return nil, false
		}
		extData := exts[:extLen]
		exts = exts[extLen:]

		if extType == extServerName {
			return parseServerNameExt(extData)
		}
	}
	return nil, false
}

func parseServerNameExt(b []byte) ([]byte, bool) {
	if len(b) < 2 {
		return nil, false
	}
	listLen := int(binary.BigEndian.Uint16(b[:2]))
	b = b[2:]
	if len(b) < listLen {
		return nil, false
	}
	b = b[:listLen]

	for len(b) >= 3 {
		nameType := b[0]
		nameLen := int(binary.BigEndian.Uint16(b[1:3]))
		b = b[3:]
		if len(b) < nameLen {
			return nil, false
		}
		name := b[:nameLen]
		b = b[nameLen:]
		if nameType == nameTypeHost && validHostname(name) {
			return name, true
		}
	}
	return nil, false
}

func validHostname(b []byte) bool {
	if len(b) == 0 || b[0] == '.' || b[len(b)-1] == '.' {
		return false
	}
	for _, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '.':
		default:
			return false
		}
	}
	return true
}

// MatchRoute looks up sni (already lower-cased) in routes, checking an exact
// match first and then a single-label wildcard "*.suffix" match. Zero allocations.
func MatchRoute[V any](routes map[string]V, sni string) (V, bool) {
	if v, ok := routes[sni]; ok {
		return v, true
	}
	dot := strings.IndexByte(sni, '.')
	if dot > 0 && dot+1 < len(sni) {
		suffix := sni[dot:] // ".example.com" — zero-alloc slice of sni
		for k, v := range routes {
			if len(k) == 1+len(suffix) && k[0] == '*' && k[1:] == suffix {
				return v, true
			}
		}
	}
	var zero V
	return zero, false
}
