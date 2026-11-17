package abi4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func testEvent() (Event, []byte) {
	block := []byte("GET /v1/items?q=1 HTTP/1.1\r\nHost: example.test\r\nX-Test: one\r\nX-Test: two\r\nContent-Length: 0\r\n\r\n")
	idx := make([]Ent, 16)
	n, start, err := SplitHeaders(block, idx)
	if err != nil {
		panic(err)
	}
	if n != 4 {
		panic("unexpected header count")
	}
	return Event{
		Block: block,
		Index: idx[:n],
		Start: &start,
		Conn: &ConnInfo{
			Flags: 1, Family: 4,
			RemotePort: 12345, LocalPort: 8080,
		},
		State: []byte("state-bytes"),
	}, block
}

func mustWrite(t *testing.T, ev Event) []byte {
	t.Helper()
	dst := make([]byte, 4096)
	n, err := Write(dst, ev)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return dst[:n]
}

func sectionOffset(t *testing.T, env []byte, typ int) int {
	t.Helper()
	le := binary.LittleEndian
	count := int(le.Uint16(env[12:]))
	for i := 0; i < count; i++ {
		w := env[24+i*secStride:]
		if int(le.Uint16(w[0:])) == typ {
			return int(le.Uint32(w[4:]))
		}
	}
	t.Fatalf("section %d not found", typ)
	return 0
}

func sectionTableOffset(t *testing.T, env []byte, typ int) int {
	t.Helper()
	le := binary.LittleEndian
	count := int(le.Uint16(env[12:]))
	for i := 0; i < count; i++ {
		w := env[24+i*secStride:]
		if int(le.Uint16(w[0:])) == typ {
			return 24 + i*secStride
		}
	}
	t.Fatalf("section %d not found", typ)
	return 0
}

func TestWriteParseRoundTrip(t *testing.T) {
	ev, block := testEvent()
	env := mustWrite(t, ev)
	if len(env) == 0 || len(env) > 4096 {
		t.Fatalf("Write returned %d", len(env))
	}

	parsed, err := Parse(env)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !bytes.Equal(parsed.Blob, block) {
		t.Fatalf("blob mismatch")
	}
	if !bytes.Equal(parsed.State, []byte("state-bytes")) {
		t.Fatalf("state mismatch: %q", parsed.State)
	}
	if parsed.Start == nil || parsed.Start.MethodLen != 3 || parsed.Start.ProtoLen != 8 {
		t.Fatalf("start line missing or mis-split: %+v", parsed.Start)
	}
	if got, ok := parsed.Header(env, "x-test"); !ok || string(got) != "one" {
		t.Fatalf("header x-test = %q, %v", got, ok)
	}
	if got, ok := parsed.Header(env, "X-Test"); !ok || string(got) != "one" {
		t.Fatalf("first duplicate must win: %q, %v", got, ok)
	}
	if _, ok := parsed.Header(env, "missing"); ok {
		t.Fatal("missing header reported as present")
	}
	if len(parsed.SectionIDs) != 5 {
		t.Fatalf("sections = %v", parsed.SectionIDs)
	}
	if parsed.Total != len(env) {
		t.Fatalf("total = %d, envelope = %d", parsed.Total, len(env))
	}
}

func TestWriteChecksCapacityBeforeWrite(t *testing.T) {
	ev, _ := testEvent()
	env := mustWrite(t, ev)

	short := bytes.Repeat([]byte{0xaa}, len(env)-1)
	before := append([]byte(nil), short...)
	if _, err := Write(short, ev); err == nil {
		t.Fatal("Write accepted a short window")
	}
	if !bytes.Equal(short, before) {
		t.Fatal("Write modified the destination before failing")
	}

	exact := make([]byte, len(env))
	if got, err := Write(exact, ev); err != nil || got != len(env) {
		t.Fatalf("Write exact = (%d, %v), want (%d, nil)", got, err, len(env))
	}
}

func TestWriteRejectsInvalidEventWithoutTouchingWindow(t *testing.T) {
	ev, block := testEvent()
	cases := []struct {
		name string
		edit func(*Event)
	}{
		{"index name out of range", func(e *Event) { e.Index[0].NameOff = uint32(len(block)) }},
		{"index value out of range", func(e *Event) { e.Index[0].ValOff = uint32(len(block)) }},
		{"empty index name", func(e *Event) { e.Index[0].NameLen = 0 }},
		{"start path out of range", func(e *Event) { e.Start.PathOff = uint32(len(block)) }},
		{"conn sni out of range", func(e *Event) { e.Conn.SNIOff = uint32(len(block)); e.Conn.SNILen = 1 }},
		{"conn alpn out of range", func(e *Event) { e.Conn.ALPNOff = uint32(len(block)); e.Conn.ALPNLen = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := ev
			bad.Index = append([]Ent(nil), ev.Index...)
			start := *ev.Start
			bad.Start = &start
			conn := *ev.Conn
			bad.Conn = &conn
			tc.edit(&bad)

			dst := bytes.Repeat([]byte{0xaa}, 4096)
			before := append([]byte(nil), dst...)
			if _, err := Write(dst, bad); err == nil {
				t.Fatal("invalid event accepted")
			}
			if !bytes.Equal(dst, before) {
				t.Fatal("failed Write modified the destination")
			}
		})
	}
}

func TestParseRejectsCorruption(t *testing.T) {
	ev, _ := testEvent()
	base := mustWrite(t, ev)
	le := binary.LittleEndian

	cases := []struct {
		name string
		edit func([]byte)
	}{
		{"magic", func(b []byte) { b[0] ^= 0xff }},
		{"version", func(b []byte) { b[4] = 2 }},
		{"header length", func(b []byte) { b[6] = 23 }},
		{"stride", func(b []byte) { b[14] = 15 }},
		{"total too large", func(b []byte) { le.PutUint32(b[8:], uint32(len(b)+8)) }},
		{"total unaligned", func(b []byte) { le.PutUint32(b[8:], uint32(len(b)-4)) }},
		{"zero sections", func(b []byte) { le.PutUint16(b[12:], 0) }},
		{"too many sections", func(b []byte) { le.PutUint16(b[12:], maxSections+1) }},
		{"arena before table", func(b []byte) { le.PutUint32(b[16:], 0) }},
		{"arena past total", func(b []byte) { le.PutUint32(b[20:], uint32(len(b))) }},
		{"section order", func(b []byte) { le.PutUint16(b[24:], secStart) }},
		{"section type zero", func(b []byte) { le.PutUint16(b[24:], 0) }},
		{"section offset into table", func(b []byte) { le.PutUint32(b[24+4:], 24) }},
		{"index count", func(b []byte) { b[sectionTableOffset(t, b, secIndex)+12] = 0xff }},
		{"index offset unaligned", func(b []byte) { le.PutUint32(b[sectionTableOffset(t, b, secIndex)+4:], 25) }},
		{"index name out of blob", func(b []byte) {
			off := sectionOffset(t, b, secIndex)
			le.PutUint32(b[off:], uint32(len(b)))
		}},
		{"index empty name", func(b []byte) {
			off := sectionOffset(t, b, secIndex)
			le.PutUint16(b[off+8:], 0)
		}},
		{"start offset out of blob", func(b []byte) {
			off := sectionOffset(t, b, secStart)
			le.PutUint32(b[off:], uint32(len(b)))
		}},
		{"conn sni out of blob", func(b []byte) {
			off := sectionOffset(t, b, secConn)
			le.PutUint32(b[off+48:], uint32(len(b)))
			le.PutUint32(b[off+52:], 1)
		}},
		{"conn alpn out of blob", func(b []byte) {
			off := sectionOffset(t, b, secConn)
			le.PutUint32(b[off+56:], uint32(len(b)))
			le.PutUint32(b[off+60:], 1)
		}},
		{"overlapping sections", func(b []byte) {
			startOff := sectionOffset(t, b, secStart)
			le.PutUint32(b[sectionTableOffset(t, b, secConn)+4:], uint32(startOff))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), base...)
			tc.edit(b)
			if _, err := Parse(b); err == nil {
				t.Fatal("corrupt envelope accepted")
			}
		})
	}
}

func TestParseRejectsStateOutsideArena(t *testing.T) {
	env := mustWrite(t, Event{Block: []byte("abc"), State: []byte("s")})
	le := binary.LittleEndian
	table := sectionTableOffset(t, env, secState)
	// Shrink the declared arena so the state section starts past its end while
	// remaining inside total_len. The old unsigned subtraction wrapped and
	// accepted this envelope.
	le.PutUint32(env[20:], 3)
	le.PutUint32(env[table+8:], 1)
	if _, err := Parse(env); err == nil {
		t.Fatal("state outside the arena accepted")
	}
}

func TestParseRejectsHugeIndexBeforeAlloc(t *testing.T) {
	ev, _ := testEvent()
	b := append([]byte(nil), mustWrite(t, ev)...)
	le := binary.LittleEndian
	off := sectionOffset(t, b, secIndex)
	table := sectionTableOffset(t, b, secIndex)
	cnt := uint32(maxEntries + 1)
	ln := cnt * entLen
	total := align8(off + int(ln))
	b = append(b, make([]byte, total-len(b))...)
	le.PutUint32(b[8:], uint32(total))
	le.PutUint32(b[table+8:], ln)
	le.PutUint32(b[table+12:], cnt)
	if _, err := Parse(b); err == nil {
		t.Fatal("oversized index accepted")
	}
}

func TestWriteParseVariants(t *testing.T) {
	block := []byte("GET / HTTP/1.1\r\nHost: x\r\nX: y\r\n\r\n")
	idx := make([]Ent, 8)
	n, start, err := SplitHeaders(block, idx)
	if err != nil {
		t.Fatal(err)
	}
	startPtr := &start
	cases := []struct {
		name  string
		ev    Event
		index int
		start bool
		conn  bool
		state bool
	}{
		{"opaque chunk", Event{Block: []byte("abc")}, 0, false, false, false},
		{"start only", Event{Block: block, Start: startPtr}, 0, true, false, false},
		{"index only", Event{Block: block, Index: idx[:n]}, n, false, false, false},
		{"state only", Event{Block: block, State: []byte("state")}, 0, false, false, true},
		{"all", Event{Block: block, Index: idx[:n], Start: startPtr, State: []byte("state"),
			Conn: &ConnInfo{Flags: 1, Family: 4, RemotePort: 1, LocalPort: 2}}, n, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := mustWrite(t, tc.ev)
			parsed, err := Parse(env)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !bytes.Equal(parsed.Blob, tc.ev.Block) {
				t.Fatalf("blob = %q", parsed.Blob)
			}
			if len(parsed.Index) != tc.index {
				t.Fatalf("index = %d, want %d", len(parsed.Index), tc.index)
			}
			if (parsed.Start != nil) != tc.start {
				t.Fatalf("start = %+v", parsed.Start)
			}
			if (parsed.Conn != nil) != tc.conn {
				t.Fatalf("conn = %+v", parsed.Conn)
			}
			if (parsed.State != nil) != tc.state {
				t.Fatalf("state = %q", parsed.State)
			}
			if parsed.Total%8 != 0 {
				t.Fatalf("total %d is not 8-byte aligned", parsed.Total)
			}
		})
	}
}

func TestSplitHeadersRequiresCRLF(t *testing.T) {
	for _, block := range [][]byte{
		[]byte("GET / HTTP/1.1\nHost: x\n\n"),
		[]byte("GET / HTTP/1.1\r\nHost: x"),
		[]byte("GET / HTTP/1.1\r\nHost: x\n\r\n"),
		[]byte("GET / HTTP/1.1\r\n\r\nextra"),
	} {
		idx := make([]Ent, 8)
		if _, _, err := SplitHeaders(block, idx); err == nil {
			t.Fatalf("invalid header block accepted: %q", block)
		}
	}
}

func TestParseSkipsUnknownSection(t *testing.T) {
	env := mustWrite(t, Event{Block: []byte("abc"), State: []byte("s")})
	le := binary.LittleEndian
	// The second section is state (type 5); re-tag it as a future type 6.
	le.PutUint16(env[24+secStride:], 6)
	parsed, err := Parse(env)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.State != nil {
		t.Fatalf("unknown section surfaced as state: %q", parsed.State)
	}
	if len(parsed.SectionIDs) != 2 || parsed.SectionIDs[1] != 6 {
		t.Fatalf("sections = %v", parsed.SectionIDs)
	}
}

func TestParseRejectsMissingBlob(t *testing.T) {
	// Rewrite the only section as an unknown type: sorted and structurally
	// valid, but no secBlob, so Parse must fail closed.
	ev, _ := testEvent()
	b := append([]byte(nil), mustWrite(t, ev)...)
	le := binary.LittleEndian
	le.PutUint16(b[24:], 6)
	le.PutUint16(b[12:], 1)
	le.PutUint32(b[20:], 0)
	if _, err := Parse(b); err == nil {
		t.Fatal("envelope without a blob section accepted")
	}
}

func TestParseHeaderFailsClosedOnShortWindow(t *testing.T) {
	ev, _ := testEvent()
	env := mustWrite(t, ev)
	parsed, err := Parse(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed.Header(env[:8], "Host"); ok {
		t.Fatal("Header accepted a short window")
	}
}

func TestSplitHeadersPreservesDuplicates(t *testing.T) {
	block := []byte("GET / HTTP/1.1\r\nX: 1\r\nX: 2\r\nEmpty:\r\n\r\n")
	idx := make([]Ent, 8)
	n, sl, err := SplitHeaders(block, idx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("headers = %d, want 3", n)
	}
	if idx[0].NameLen != 1 || idx[0].ValLen != 1 || idx[1].ValLen != 1 || idx[2].ValLen != 0 {
		t.Fatalf("unexpected index: %+v", idx[:n])
	}
	if sl.MethodLen != 3 || sl.PathLen != 1 || sl.ProtoLen != 8 {
		t.Fatalf("start line = %+v", sl)
	}
}

func TestSplitHeadersRejectsMalformed(t *testing.T) {
	long := bytes.Repeat([]byte("a"), maxU16+1)
	cases := [][]byte{
		nil,
		[]byte("\r\n"),
		[]byte("GET\r\n"),
		[]byte("GET /\r\n"),
		[]byte("GET  HTTP/1.1\r\n\r\n"),
		[]byte("GET / HTTP/1.1 extra\r\n\r\n"),
		[]byte("GET / HTTP/1.1\r\nBadHeader\r\n\r\n"),
		[]byte("GET / HTTP/1.1\r\n: value\r\n\r\n"),
		[]byte("GET / HTTP/1.1\r\nBad Name: value\r\n\r\n"),
		[]byte("GET / HTTP/1.1\r\nX: a\x00b\r\n\r\n"),
		[]byte("GET / HTTP/1.1\r\nX: a\rb\r\n\r\n"),
		append(append([]byte("GET / HTTP/1.1\r\nX: "), long...), []byte("\r\n\r\n")...),
	}
	for _, block := range cases {
		idx := make([]Ent, 8)
		if _, _, err := SplitHeaders(block, idx); err == nil {
			t.Fatalf("malformed block accepted: %q", block)
		}
	}
}

func TestWriteRejectsOversizedIndex(t *testing.T) {
	block := []byte("GET / HTTP/1.1\r\nA: b\r\n\r\n")
	idx := make([]Ent, maxEntries+1)
	for i := range idx {
		idx[i] = Ent{NameOff: 0, NameLen: 1, ValOff: 0, ValLen: 0}
	}
	dst := bytes.Repeat([]byte{0xaa}, 4096)
	before := append([]byte(nil), dst...)
	if _, err := Write(dst, Event{Block: block, Index: idx}); !errors.Is(err, errBadCount) {
		t.Fatalf("err = %v, want %v", err, errBadCount)
	}
	if !bytes.Equal(dst, before) {
		t.Fatal("failed Write modified the destination")
	}
}

func TestSplitHeadersRejectsTooMany(t *testing.T) {
	block := []byte("GET / HTTP/1.1\r\n")
	for i := 0; i < 128; i++ {
		block = append(block, []byte("X: y\r\n")...)
	}
	block = append(block, '\r', '\n')
	idx := make([]Ent, 128)
	n, _, err := SplitHeaders(block, idx)
	if err != nil || n != 128 {
		t.Fatalf("exact index capacity = (%d, %v), want (128, nil)", n, err)
	}

	block = append(block[:len(block)-2], []byte("X: y\r\n\r\n")...)
	if _, _, err := SplitHeaders(block, idx); !errors.Is(err, errTooManyHeaders) {
		t.Fatalf("err = %v, want %v", err, errTooManyHeaders)
	}
}

func TestSplitHeadersZeroAlloc(t *testing.T) {
	block := []byte("GET /v1/items?q=1 HTTP/1.1\r\nHost: example.test\r\nX-Test: one\r\nContent-Length: 0\r\n\r\n")
	idx := make([]Ent, 128)
	allocs := testing.AllocsPerRun(1000, func() {
		if _, _, err := SplitHeaders(block, idx); err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("SplitHeaders allocates %.1f times per call", allocs)
	}
}

func FuzzWriteParse(f *testing.F) {
	ev, _ := testEvent()
	env := make([]byte, 4096)
	n, _ := Write(env, ev)
	f.Add(env[:n], []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	f.Add([]byte{}, []byte(""))
	f.Fuzz(func(_ *testing.T, win, block []byte) {
		idx := make([]Ent, 8)
		n, start, err := SplitHeaders(block, idx)
		if err != nil {
			n = 0
			start = StartLine{}
		}
		_, _ = Write(make([]byte, len(win)), Event{
			Block: block, Index: idx[:n], Start: &start,
		})
		_, _ = Parse(win)
	})
}

func FuzzSplitHeaders(f *testing.F) {
	f.Add([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	f.Add([]byte(""))
	f.Fuzz(func(_ *testing.T, block []byte) {
		idx := make([]Ent, 16)
		_, _, _ = SplitHeaders(block, idx)
	})
}

func FuzzParse(f *testing.F) {
	ev, _ := testEvent()
	env := make([]byte, 4096)
	n, _ := Write(env, ev)
	f.Add(env[:n])
	f.Add([]byte{})
	f.Fuzz(func(_ *testing.T, win []byte) {
		_, _ = Parse(win)
	})
}
