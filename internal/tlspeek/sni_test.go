package tlspeek

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

// buildClientHello constructs a minimal valid TLS 1.2 ClientHello record with
// the given SNI hostname.
func buildClientHello(sni string) []byte {
	host := []byte(sni)
	// server_name extension payload:
	//   2B list length + 1B type (0) + 2B host length + host
	sniExtData := make([]byte, 2+1+2+len(host))
	listLen := 1 + 2 + len(host)
	sniExtData[0] = byte(listLen >> 8)
	sniExtData[1] = byte(listLen)
	sniExtData[2] = 0x00 // host_name
	sniExtData[3] = byte(len(host) >> 8)
	sniExtData[4] = byte(len(host))
	copy(sniExtData[5:], host)

	// Extension header (4B): type 0x0000 + length
	ext := make([]byte, 4+len(sniExtData))
	ext[2] = byte(len(sniExtData) >> 8)
	ext[3] = byte(len(sniExtData))
	copy(ext[4:], sniExtData)

	// ClientHello body:
	//   2B version (0x0303) + 32B random + 1B sid len (0)
	//   + 2B cipher suites len (2) + 2B cipher suite (0x002f)
	//   + 1B comp methods len (1) + 1B null comp (0x00)
	//   + 2B extensions len + ext
	ch := make([]byte, 0, 64+len(ext))
	ch = append(ch, 0x03, 0x03)
	ch = append(ch, make([]byte, 32)...)
	ch = append(ch, 0x00)                   // session_id len = 0
	ch = append(ch, 0x00, 0x02, 0x00, 0x2f) // 1 cipher suite
	ch = append(ch, 0x01, 0x00)             // 1 compression method
	ch = append(ch, byte(len(ext)>>8), byte(len(ext)))
	ch = append(ch, ext...)

	// Handshake header: 1B type (0x01) + 3B length
	hs := make([]byte, 4+len(ch))
	hs[0] = 0x01
	hs[1] = byte(len(ch) >> 16)
	hs[2] = byte(len(ch) >> 8)
	hs[3] = byte(len(ch))
	copy(hs[4:], ch)

	// TLS record header: 1B type (0x16) + 2B version (0x0301) + 2B length
	rec := make([]byte, 5+len(hs))
	rec[0] = 0x16
	rec[1] = 0x03
	rec[2] = 0x01
	rec[3] = byte(len(hs) >> 8)
	rec[4] = byte(len(hs))
	copy(rec[5:], hs)
	return rec
}

func TestParseClientHelloSNI(t *testing.T) {
	rec := buildClientHello("api.example.com")
	got, ok := ParseClientHelloSNI(rec)
	if !ok || string(got) != "api.example.com" {
		t.Fatalf("ParseClientHelloSNI = (%q, %v), want api.example.com", got, ok)
	}
}

func TestParseClientHelloRejectsMalformed(t *testing.T) {
	rec := buildClientHello("api.example.com")
	for cut := 0; cut < len(rec); cut++ {
		_, _ = ParseClientHelloSNI(rec[:cut]) // must never panic on truncated input
	}
	if _, ok := ParseClientHelloSNI([]byte("GET / HTTP/1.1\r\n")); ok {
		t.Fatal("HTTP request parsed as TLS ClientHello")
	}
}

func TestPeekRealStdlibTLSAndReplay(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Have crypto/tls send a real ClientHello with ServerName "svc.internal.example".
	go func() {
		cli := tls.Client(c1, &tls.Config{
			ServerName:         "SVC.Internal.Example",
			InsecureSkipVerify: true, //nolint:gosec // test fixture
		})
		_ = cli.SetDeadline(time.Now().Add(500 * time.Millisecond))
		_ = cli.Handshake()
	}()

	sni, wrapped, err := Peek(c2, time.Second)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if sni != "svc.internal.example" {
		t.Fatalf("sni = %q, want svc.internal.example", sni)
	}

	// DrainTo must replay the entire ClientHello record so the upstream TLS
	// server sees every byte the client sent.
	var replayed bytes.Buffer
	if _, n, err := wrapped.(*Conn).DrainTo(&replayed); err != nil || n == 0 {
		t.Fatalf("DrainTo: n=%d, err=%v", n, err)
	}
	if got, ok := ParseClientHelloSNI(replayed.Bytes()); !ok || string(got) != "SVC.Internal.Example" {
		t.Fatalf("replayed bytes did not contain intact ClientHello: ok=%v, got=%q", ok, got)
	}
}

func TestPeekNonTLSReplaysBytes(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	const msg = "HELLO-PLAIN-TCP"
	go func() {
		_, _ = c1.Write([]byte(msg))
		_ = c1.Close()
	}()

	sni, wrapped, err := Peek(c2, time.Second)
	if err != nil {
		t.Fatalf("Peek plain TCP: %v", err)
	}
	if sni != "" {
		t.Fatalf("sni = %q on plain TCP, want empty", sni)
	}
	got, err := io.ReadAll(wrapped)
	if err != nil || string(got) != msg {
		t.Fatalf("replayed plain TCP = %q (err=%v), want %q", got, err, msg)
	}
}

func TestMatchRouteExactAndWildcard(t *testing.T) {
	routes := map[string]string{
		"api.example.com": "api_pool",
		"*.example.com":   "wild_pool",
	}
	if got, ok := MatchRoute(routes, "api.example.com"); !ok || got != "api_pool" {
		t.Fatalf("exact match = (%q, %v), want api_pool", got, ok)
	}
	if got, ok := MatchRoute(routes, " billing.example.com"[1:]); !ok || got != "wild_pool" {
		t.Fatalf("wildcard match = (%q, %v), want wild_pool", got, ok)
	}
	if _, ok := MatchRoute(routes, "other.org"); ok {
		t.Fatal("unexpected match for other.org")
	}
}

func FuzzParseClientHelloSNI(f *testing.F) {
	f.Add(buildClientHello("api.example.com"))
	f.Add([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = ParseClientHelloSNI(data)
	})
}

func BenchmarkParseClientHelloSNI(b *testing.B) {
	rec := buildClientHello("api.example.com")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := ParseClientHelloSNI(rec); !ok {
			b.Fatal("expected ok")
		}
	}
}

func BenchmarkMatchRouteWildcard(b *testing.B) {
	routes := map[string]int{
		"api.example.com": 1,
		"*.example.com":   2,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if v, ok := MatchRoute(routes, "sub.example.com"); !ok || v != 2 {
			b.Fatal("expected wildcard match")
		}
	}
}
