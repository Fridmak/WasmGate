package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestJoinRelaysBothDirections(t *testing.T) {
	a, aPeer := net.Pipe()
	b, bPeer := net.Pipe()
	defer aPeer.Close()
	defer bPeer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Join(ctx, a, b, 0, nil, nil, 1)
	}()

	req := []byte("request")
	resp := []byte("response")
	go func() { _, _ = aPeer.Write(req) }()
	go func() { _, _ = bPeer.Write(resp) }()

	gotReq := make([]byte, len(req))
	if _, err := io.ReadFull(bPeer, gotReq); err != nil {
		t.Fatalf("read request: %v", err)
	}
	gotResp := make([]byte, len(resp))
	if _, err := io.ReadFull(aPeer, gotResp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(gotReq) != string(req) || string(gotResp) != string(resp) {
		t.Fatalf("relay = %q / %q", gotReq, gotResp)
	}

	_ = aPeer.Close()
	_ = bPeer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Join did not return after both peers closed")
	}
}

func TestCopyInspectedAllowsAndDenies(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		deny bool
	}{
		{name: "allow", data: "hello"},
		{name: "deny", data: "bad", deny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &scriptedConn{reads: []string{tc.data}}
			dst := &scriptedConn{}
			calls := 0
			insp := func(_ context.Context, _ uint64, _ int32, chunk []byte) (int32, error) {
				calls++
				if string(chunk) == "bad" {
					return 1, nil
				}
				return 0, nil
			}
			_, err := copyInspected(context.Background(), dst, src, 0, insp, nil, 1, 0)
			if tc.deny {
				if !errors.Is(err, errPluginDenied) || dst.buf.Len() != 0 {
					t.Fatalf("deny = (%v, %q)", err, dst.buf.String())
				}
			} else if err != nil || dst.buf.String() != tc.data || calls != 1 {
				t.Fatalf("allow = (%v, %q, %d calls)", err, dst.buf.String(), calls)
			}
		})
	}
}

func TestCopyPlainAndWatched(t *testing.T) {
	for _, idle := range []time.Duration{0, 20 * time.Millisecond} {
		t.Run(idle.String(), func(t *testing.T) {
			src := &scriptedConn{reads: []string{"payload"}}
			dst := &scriptedConn{}
			n, err := copyConn(dst, src, idle)
			if err != nil || n != int64(len("payload")) || dst.buf.String() != "payload" {
				t.Fatalf("copyConn = (%d, %v, %q)", n, err, dst.buf.String())
			}
		})
	}
}

func TestBufferPoolReuse(t *testing.T) {
	buf := GetBuffer()
	if cap(*buf) < IdleBufferSize {
		t.Fatalf("buffer capacity = %d, want at least %d", cap(*buf), IdleBufferSize)
	}
	PutBuffer(buf)
}

func TestHalfCloseWriteClosesPlainConnection(t *testing.T) {
	c, peer := net.Pipe()
	defer peer.Close()

	HalfCloseWrite(c)
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("plain connection remained open after half-close")
	}
}

type scriptedConn struct {
	reads []string
	buf   bytes.Buffer
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	if len(c.reads) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.reads[0])
	c.reads[0] = c.reads[0][n:]
	if c.reads[0] == "" {
		c.reads = c.reads[1:]
	}
	return n, nil
}

func (c *scriptedConn) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *scriptedConn) Close() error                { return nil }
func (c *scriptedConn) LocalAddr() net.Addr         { return testAddr{} }
func (c *scriptedConn) RemoteAddr() net.Addr        { return testAddr{} }
func (c *scriptedConn) SetDeadline(time.Time) error { return nil }

func (c *scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr struct{}

func (testAddr) Network() string { return "test" }
func (testAddr) String() string  { return "test" }
