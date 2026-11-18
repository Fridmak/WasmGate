package engine

import (
	"context"
	"net"
	"path/filepath"
	"testing"
)

func TestReferenceGuests(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		plan    Plan
		routes  int
		payload string
		want    Verdict
	}{
		{name: "ipguard", plan: InspectLifecycle},
		{"http_guard", InspectRequestHeaders, 0, "GET / HTTP/1.1\r\nHost: example.test\r\nAuthorization: Basic dXNlcjpwYXNz\r\n\r\n", VerdictAllow},
		{"http_headers_guard", InspectRequestHeaders, 0, "GET /sample HTTP/1.1\r\nHost: example.test\r\n\r\n", VerdictAllow},
		{"http_body_guard", InspectRequestHeaders | InspectRequestBody | InspectResponseHeaders | InspectResponseBody, 0,
			"POST / HTTP/1.1\r\nHost: example.test\r\nContent-Length: 5\r\n\r\n", VerdictAllow},
		{"path_router", InspectRequestHeaders, 3, "GET / HTTP/1.1\r\nHost: api.example.test\r\n\r\n", 2},
		{"sni_guard", InspectOpaqueStream, 3, "not TLS", VerdictAllow},
		{"protocol_router", InspectOpaqueStream, 5, "GET / HTTP/1.1\r\n", 3},
		{"redis_guard", InspectOpaqueStream, 0, "PING\r\n", VerdictAllow},
		{"stream_guard", InspectOpaqueStream, 0, "hello", VerdictAllow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join("..", "..", "..", "..", "plugins", "examples", tc.name, tc.name+".wasm")
			e, err := LoadFile(ctx, path, Options{Plan: tc.plan, RoutePools: tc.routes})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close(ctx)
			var v Verdict
			switch {
			case tc.plan.HasLifecycle():
				if v, err = e.OnAccept(ctx, net.IPv4(127, 0, 0, 99), 1234); err != nil || v != VerdictDeny {
					t.Fatalf("example lifecycle deny = %d, %v", v, err)
				}
				v, err = e.OnAccept(ctx, net.IPv4(127, 0, 0, 1), 1234)
			case tc.plan.Has(PhaseRequestHeaders):
				v, err = e.OnRequestHeaders(ctx, 1, []byte(tc.payload))
			default:
				v, err = e.OnOpaqueFirst(ctx, 1, []byte(tc.payload))
			}
			if err != nil || v != tc.want {
				t.Fatalf("event = (%d, %v), want %d", v, err, tc.want)
			}
		})
	}
}
