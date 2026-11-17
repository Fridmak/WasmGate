package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/plugin/abi4"
)

const (
	bodyTestCaps = abi4.CapBlob | abi4.CapHeaderIndex | abi4.CapStartLine | abi4.CapStateIn | abi4.CapStateOut
	bodyTestPlan = abi4.InspectRequestHeaders | abi4.InspectRequestBody | abi4.InspectResponseHeaders | abi4.InspectResponseBody
)

func TestContentLengthFraming(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fields  string
		length  int64
		present bool
		invalid bool
	}{
		{name: "absent"},
		{name: "zero", fields: "Content-Length: 0\r\n", present: true},
		{name: "positive", fields: "cOnTeNt-LeNgTh:\t 12\t\r\n", length: 12, present: true},
		{name: "transfer encoding", fields: "Transfer-Encoding: chunked\r\n", invalid: true},
		{name: "duplicate", fields: "Content-Length: 1\r\nContent-Length: 1\r\n", invalid: true},
		{name: "negative", fields: "Content-Length: -1\r\n", invalid: true},
		{name: "plus", fields: "Content-Length: +1\r\n", invalid: true},
		{name: "empty", fields: "Content-Length:\r\n", invalid: true},
		{name: "overflow", fields: "Content-Length: 9223372036854775808\r\n", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := []byte("POST / HTTP/1.1\r\n" + tc.fields + "\r\n")
			var index [4]abi4.Ent
			n, _, err := abi4.SplitHeaders(head, index[:])
			if err != nil {
				t.Fatal(err)
			}
			got, present, err := contentLength(head, index[:n])
			if (err != nil) != tc.invalid || (!tc.invalid && (got != tc.length || present != tc.present)) {
				t.Fatalf("content length = (%d, %v, %v)", got, present, err)
			}
		})
	}
}

func httpTestEngine(t *testing.T, wasm []byte, plan abi4.Plan) *abi4.Engine {
	t.Helper()
	ctx := context.Background()
	e, err := abi4.New(ctx, wasm, abi4.Options{Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(ctx) })
	return e
}

func TestResponseBodyFraming(t *testing.T) {
	const fields = "X-Test: test\r\n"
	engine := httpTestEngine(t, v4Verdict(0), bodyTestPlan)
	for _, tc := range []struct {
		name, head string
		headMethod bool
		length     int64
		invalid    bool
	}{
		{name: "explicit zero", head: "200 OK\r\nContent-Length: 0\r\n"},
		{name: "has length", head: "200 Everything Fine\r\nContent-Length: 28\r\n", length: 28},
		{name: "HEAD", head: "200 OK\r\nContent-Length: 99\r\n", headMethod: true},
		{name: "no content", head: "204 No Content\r\n"},
		{name: "reset content", head: "205 Reset Content\r\n"},
		{name: "not modified", head: "304 Not Modified\r\nContent-Length: 99\r\n"},
		{name: "missing length", head: "200 OK\r\n", invalid: true},
		{name: "informational unsupported", head: "103 Early Hints\r\n", invalid: true},
		{name: "no content with length", head: "204 No Content\r\nContent-Length: 0\r\n", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newHTTPExchange(engine, 1, config.PluginConfig{}, false, nil)
			x.headRequest = tc.headMethod
			err := x.prepareResponse(context.Background(), []byte("HTTP/1.1 "+tc.head+fields+"\r\n"))
			if (err != nil) != tc.invalid || (!tc.invalid && x.response.left != tc.length) {
				t.Fatalf("response length = (%d, %v)", x.response.left, err)
			}
		})
	}
}

func TestBodyLengthLimit(t *testing.T) {
	engine := httpTestEngine(t, v4Verdict(0), bodyTestPlan)
	pc := config.PluginConfig{MaxBodyBytes: 10}

	x := newHTTPExchange(engine, 1, pc, false, nil)
	_, err := x.prepareRequest(context.Background(),
		[]byte("POST / HTTP/1.1\r\nContent-Length: 11\r\n\r\n"), nil)
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("oversized request = %v, want %v", err, errBodyTooLarge)
	}

	x = newHTTPExchange(engine, 1, pc, false, nil)
	if err := x.prepareResponse(context.Background(),
		[]byte("HTTP/1.1 200 OK\r\nContent-Length: 11\r\n\r\n")); !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("oversized response = %v, want %v", err, errBodyTooLarge)
	}

	x = newHTTPExchange(engine, 1, pc, false, nil)
	if err := x.prepareResponse(context.Background(),
		[]byte("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n")); err != nil {
		t.Fatalf("boundary response: %v", err)
	}
	if x.response.left != 10 {
		t.Fatalf("boundary response length = %d, want 10", x.response.left)
	}
}

func TestBodyFlowRejectsInvalidReplacementBeforeStateCommit(t *testing.T) {
	wasm := fixtureBodyGuest(t, abi4.PhaseRequestBody, nil, abi4.Output{Mutation: []byte("longer"), State: []byte("new")}, abi4.VerdictAllow)
	e := httpTestEngine(t, wasm, bodyTestPlan)
	b := newBodyFlow(e, 1, abi4.PhaseRequestBody, true, nil)
	b.left = 5
	if err := b.state.Commit([]byte("old")); err != nil {
		t.Fatal(err)
	}
	data := []byte("hello")
	if err := b.accept(context.Background(), data, nil); !errors.Is(err, errBodyMutation) {
		t.Fatalf("replacement = %v, want %v", err, errBodyMutation)
	}
	if string(data) != "hello" || string(b.state.Bytes()) != "old" || b.left != 5 {
		t.Fatalf("rejected event changed the body or its state: %q, %q, %d", data, b.state.Bytes(), b.left)
	}
}

func TestReplacementRequiresHostPolicy(t *testing.T) {
	wasm := fixtureBodyGuest(t, abi4.PhaseRequestBody, nil, abi4.Output{Mutation: []byte("HELLO")}, abi4.VerdictAllow)
	x := newHTTPExchange(httpTestEngine(t, wasm, bodyTestPlan), 1, config.PluginConfig{}, false, nil)
	body := []byte("hello")
	_, err := x.prepareRequest(context.Background(), []byte("POST / HTTP/1.1\r\nContent-Length: 5\r\n\r\n"), body)
	if !errors.Is(err, errBodyMutationPolicy) || string(body) != "hello" {
		t.Fatalf("no mutation policy: body=%q, err=%v", body, err)
	}
	pc := config.PluginConfig{Wasm: v4Verdict(0), DataMode: config.DataModeHeaders, BodyPolicy: config.BodyPolicyRequest,
		MutationPolicy: config.MutationPolicyReplace}
	if _, err := newRegistry().preparePlugin("edge", pc); err == nil {
		t.Fatal("replace policy loaded a guest without CapMutation")
	}
}

func TestBodyFlowFlags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		left    int64
		started bool
		chunk   string
		readErr error
		flags   abi4.Flags
	}{
		{"single", 2, false, "ab", nil, abi4.FlagFirst | abi4.FlagLast},
		{"first", 6, false, "ab", nil, abi4.FlagFirst},
		{"middle", 4, true, "cd", nil, 0},
		{"last", 2, true, "ef", io.EOF, abi4.FlagLast},
		{"data and EOF", 3, false, "ab", io.EOF, abi4.FlagFirst | abi4.FlagLast | abi4.FlagTruncated},
		{"empty EOF", 2, true, "", io.EOF, abi4.FlagLast | abi4.FlagTruncated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, phase := range []abi4.Phase{abi4.PhaseRequestBody, abi4.PhaseResponseBody} {
				check := fixtureCheck([]byte{0x20, 0x02}, int32(tc.flags)) // local.get flags
				wasm := fixtureBodyGuest(t, phase, check, abi4.Output{State: []byte("next")}, abi4.VerdictAllow)
				e := httpTestEngine(t, wasm, bodyTestPlan)
				b := newBodyFlow(e, 1, phase, false, nil)
				b.left, b.started = tc.left, tc.started
				err := b.accept(context.Background(), []byte(tc.chunk), tc.readErr)
				if tc.flags&abi4.FlagTruncated != 0 {
					if !errors.Is(err, io.ErrUnexpectedEOF) || len(b.state.Bytes()) != 0 {
						t.Fatalf("phase %d truncation = (%v, state %q)", phase, err, b.state.Bytes())
					}
				} else if err != nil || string(b.state.Bytes()) != "next" {
					t.Fatalf("phase %d flags = (%v, state %q)", phase, err, b.state.Bytes())
				}
			}
		})
	}
}

// scriptedHTTPConn makes fragmentation and header/body coalescing deterministic
// without relying on how the kernel segments a TCP write.
type scriptedHTTPConn struct {
	net.Conn
	reads []string
	wrote bytes.Buffer
}

func (c *scriptedHTTPConn) Read(p []byte) (int, error) {
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

func (c *scriptedHTTPConn) Write(p []byte) (int, error)     { return c.wrote.Write(p) }
func (c *scriptedHTTPConn) SetReadDeadline(time.Time) error { return nil }

func TestResponseReplayUsesIndexedHeadersAndMutatedBody(t *testing.T) {
	const head = "HTTP/1.1 200 Everything Fine\r\nX-Test: yes\r\nContent-Length: 5\r\n\r\n"
	// Load the first indexed header's value and the status field from the IN
	// envelope using their absolute offsets. A missing index/start line denies.
	check := fixtureCheck(fixtureLoadRef(44, 4), 'y') // section 2 -> first entry -> value
	check = append(check, fixtureCheck(fixtureLoadRef(60, 4), '2')...)
	check = append(check, fixtureCheck([]byte{0x20, 0x02}, int32(abi4.FlagFirst|abi4.FlagLast))...)
	body := make([]byte, 1, 256) // first byte: on_event has no locals
	body = append(body, fixturePhase(abi4.PhaseResponseHeaders, check, fixtureOutput(t, abi4.Output{State: []byte("R")}), abi4.VerdictAllow)...)
	check = fixtureCheck(fixtureLoadRef(44, -1), 'R') // response headers seeded this direction's state
	body = append(body, fixturePhase(abi4.PhaseResponseBody, check, fixtureOutput(t, abi4.Output{Mutation: []byte("HELLO")}), abi4.VerdictAllow)...)
	wasm := v4ModuleCaps(append(body, 0x42, 0, 0x0b), uint64(bodyTestCaps|abi4.CapMutation))
	x := newHTTPExchange(httpTestEngine(t, wasm, bodyTestPlan), 1, config.PluginConfig{MaxHeaderBytes: len(head), InspectTimeout: time.Second}, true, nil)
	server := &scriptedHTTPConn{reads: []string{head[:len(head)-2], head[len(head)-2:] + "hello"}}
	client := &scriptedHTTPConn{}
	n := x.relayResponse(context.Background(), client, server, func() {})
	if n != int64(len(head)+5) || client.wrote.String() != head+"HELLO" {
		t.Fatalf("response = %q (%d bytes)", client.wrote.String(), n)
	}
}

func TestResponseRejectionDoesNotForwardPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, length, body string
		deny               bool
	}{
		{name: "missing length", body: "hello"},
		{name: "coalesced surplus", length: "Content-Length: 4\r\n", body: "hello"},
		{name: "denied head", length: "Content-Length: 5\r\n", body: "hello", deny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wasm := v4Verdict(0)
			if tc.deny {
				wasm = fixtureBodyGuest(t, abi4.PhaseResponseHeaders, nil, abi4.Output{}, abi4.VerdictDeny)
			}
			x := newHTTPExchange(httpTestEngine(t, wasm, bodyTestPlan), 1, config.PluginConfig{MaxHeaderBytes: 256, InspectTimeout: time.Second}, false, nil)
			server := &scriptedHTTPConn{reads: []string{"HTTP/1.1 200 OK\r\n", tc.length + "\r\n" + tc.body}}
			client := &scriptedHTTPConn{}
			if n := x.relayResponse(context.Background(), client, server, func() {}); n != 0 || client.wrote.Len() != 0 {
				t.Fatalf("rejected response forwarded %d bytes: %q", n, client.wrote.String())
			}
		})
	}
}

func TestSplitResponseHeadUsesInspectDeadline(t *testing.T) {
	up, writer := net.Pipe()
	defer up.Close()
	defer writer.Close()
	go func() { _, _ = io.WriteString(writer, "HTTP/1.1 200 OK\r\n") }()
	_, err := readResponsePrefix(up, make([]byte, 128), 128, 20*time.Millisecond, func() {})
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("response head read = %v, want inspect_timeout", err)
	}
}

func TestBodyPolicySingleExchange(t *testing.T) {
	const req = "POST / HTTP/1.1\r\nHost: example.test\r\nContent-Length: 5\r\n\r\n"
	const resp = "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: keep-alive\r\n\r\nworld"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	type upstreamResult struct {
		request string
		err     error
	}
	got := make(chan upstreamResult, 1)
	respond := make(chan struct{})
	t.Cleanup(func() { close(respond) })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- upstreamResult{err: err}
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		// The request is complete at Content-Length, without waiting for FIN.
		request := make([]byte, len(req)+5)
		_, err = io.ReadFull(c, request)
		got <- upstreamResult{request: string(request), err: err}
		if err == nil {
			_, _ = io.WriteString(c, resp)
			<-respond // do not rely on upstream EOF to end this response
		}
	}()
	wasm := fixtureBodyGuest(t, abi4.PhaseRequestBody, nil, abi4.Output{Mutation: []byte("HELLO")}, abi4.VerdictAllow)
	cfg := validated(t, edgeConfig([]string{ln.Addr().String()}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: wasm, DataMode: config.DataModeHeaders, BodyPolicy: config.BodyPolicyBoth,
			MutationPolicy: config.MutationPolicyReplace, Timeout: time.Second}
	}))
	c := dial(t, startServer(t, NewServer(testLogger(), NewRuntime(cfg), nil)))
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	writeSplit(t, c, req+"hello")
	select {
	case r := <-got:
		if r.err != nil || r.request != req+"HELLO" {
			t.Fatalf("upstream request = (%q, %v)", r.request, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not finish the first request")
	}
	data, err := io.ReadAll(c)
	if err != nil || string(data) != resp {
		t.Fatalf("response with upstream kept open = (%q, %v)", data, err)
	}
}

func TestRustGuestReplacesResponseBodyAcrossChunks(t *testing.T) {
	const bodyLen = 6 * 1024
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-WG-Mask-Digits", "yes")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", "6144")
		_, _ = io.WriteString(w, strings.Repeat("a1", bodyLen/2))
	}))
	t.Cleanup(backend.Close)
	cfg := validated(t, edgeConfig([]string{backend.Listener.Addr().String()}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Path: "../../plugins/examples/http_body_guard/http_body_guard.wasm", Timeout: time.Second,
			DataMode: config.DataModeHeaders, BodyPolicy: config.BodyPolicyBoth, MutationPolicy: config.MutationPolicyReplace}
	}))
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), nil))
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != strings.Repeat("a*", bodyLen/2) || resp.StatusCode != http.StatusOK {
		t.Fatalf("response did not preserve framing and replace all chunks: status=%d, length=%d, error=%v", resp.StatusCode, len(got), err)
	}
}

func TestBodyFlowDoesNotReadTheNextMessage(t *testing.T) {
	const next = "GET /next HTTP/1.1\r\nHost: example.test\r\n\r\n"
	ctx := context.Background()
	for _, coalesced := range []bool{false, true} {
		reads := []string{"hello", next}
		if coalesced {
			reads = []string{"hello" + next}
		}
		src, dst := &scriptedHTTPConn{reads: reads}, &scriptedHTTPConn{}
		b := bodyFlow{left: 5}
		buf := make([]byte, 128)
		_, err := b.relay(ctx, dst, src, buf, func() {})
		if coalesced {
			if !errors.Is(err, errBodyFraming) || dst.wrote.Len() != 0 {
				t.Fatalf("read crossing boundary forwarded %q or returned wrong error: %v", dst.wrote.String(), err)
			}
		} else if err != nil || dst.wrote.String() != "hello" || len(src.reads) != 1 || src.reads[0] != next {
			t.Fatalf("request boundary missed: %q, remaining %v, %v", dst.wrote.String(), src.reads, err)
		}
	}
}

func TestBodyPolicyRequiresHeaders(t *testing.T) {
	for _, mode := range []string{config.DataModeOnce, config.DataModeStream, ""} {
		cfg := edgeConfig([]string{"127.0.0.1:1"}, func(lc *config.ListenerConfig) {
			lc.Plugin = config.PluginConfig{BodyPolicy: config.BodyPolicyRequest, DataMode: mode}
		})
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "body_policy") {
			t.Fatalf("body policy with data_mode %q = %v", mode, err)
		}
	}
}

func fixtureBodyGuest(t *testing.T, phase abi4.Phase, check []byte, result abi4.Output, v abi4.Verdict) []byte {
	t.Helper()
	caps := bodyTestCaps
	if result.MutationPresent || len(result.Mutation) != 0 {
		caps |= abi4.CapMutation
	}
	body := append([]byte{0}, fixturePhase(phase, check, fixtureOutput(t, result), v)...)
	return v4ModuleCaps(append(body, 0x42, 0, 0x0b), uint64(caps))
}

func fixturePhase(phase abi4.Phase, check, stores []byte, v abi4.Verdict) []byte {
	body := []byte{0x20, 0x01, 0x41, byte(phase), 0x46, 0x04, 0x40} // if phase matches
	body = append(body, check...)
	body = append(body, stores...)
	return append(body, 0x42, byte(v), 0x0f, 0x0b) // return verdict; end if
}

func fixtureCheck(expression []byte, want int32) []byte {
	body := append(expression, 0x41) // i32.const
	body = append(body, v4SLEB(want)...)
	return append(body, 0x47, 0x04, 0x40, 0x42, 0x01, 0x0f, 0x0b) // if !=, return deny
}

func fixtureLoadRef(sectionOff, fieldOff int) []byte {
	body := []byte{0x20, 0x03, 0x28, 0x02, byte(sectionOff), 0x20, 0x03, 0x6a} // base + section.offset
	if fieldOff >= 0 {
		body = append(body, 0x28, 0x02, byte(fieldOff), 0x20, 0x03, 0x6a) // base + absolute field offset
	}
	return append(body, 0x2d, 0, 0) // i32.load8_u
}

func fixtureOutput(t *testing.T, result abi4.Output) (stores []byte) {
	t.Helper()
	var win [abi4.DefaultOutCap]byte
	n, err := abi4.WriteOutput(win[:], result)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < n; off += 4 {
		stores = append(stores, 0x41)
		stores = append(stores, v4SLEB(v4FixtureOutPtr+int32(off))...)
		stores = append(stores, 0x41)
		stores = append(stores, v4SLEB(int32(binary.LittleEndian.Uint32(win[off:])))...)
		stores = append(stores, 0x36, 0x02, 0x00)
	}
	return stores
}
