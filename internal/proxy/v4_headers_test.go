package proxy

// Hand-assembled ABI v4 modules used by proxy tests and benchmarks. Keeping
// them in Go avoids a Rust toolchain in the hot-path test suite.

func v4ULEB(n uint32) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}

func v4Section(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, v4ULEB(uint32(len(payload)))...)
	return append(out, payload...)
}

func v4Entry(body []byte) []byte {
	out := v4ULEB(uint32(len(body)))
	return append(out, body...)
}

// v4Module builds a minimal ABI v4 module exporting memory, on_event, wg_abi,
// wg_caps, wg_buf_ptr and wg_buf_len. on_event returns the supplied i64 and the
// module declares all L7 capabilities (header + opaque).
func v4Module(onEvent []byte) []byte {
	return v4ModuleCaps(onEvent, 0x7f)
}

func v4ModuleCaps(onEvent []byte, caps uint64) []byte {
	types := []byte{
		0x03,
		0x60, 0x05, 0x7e, 0x7f, 0x7f, 0x7f, 0x7f, 0x01, 0x7e,
		0x60, 0x00, 0x01, 0x7f,
		0x60, 0x00, 0x01, 0x7e,
	}
	funcs := []byte{0x05, 0x00, 0x01, 0x02, 0x01, 0x01}
	memory := []byte{0x01, 0x00, 0x01}

	exports := []byte{0x06}
	addExport := func(name string, idx uint32) {
		exports = append(exports, v4ULEB(uint32(len(name)))...)
		exports = append(exports, name...)
		exports = append(exports, 0x00)
		exports = append(exports, v4ULEB(idx)...)
	}
	exports = append(exports, 6, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00)
	addExport("on_event", 0)
	addExport("wg_abi", 1)
	addExport("wg_caps", 2)
	addExport("wg_buf_ptr", 3)
	addExport("wg_buf_len", 4)

	intConst := func(op byte, v uint32) []byte {
		body := []byte{0x00, op}
		body = append(body, v4ULEB(v)...)
		return append(body, 0x0b)
	}
	i64Const := func(v uint64) []byte {
		body := []byte{0x00, 0x42}
		body = append(body, v4ULEB(uint32(v))...)
		return append(body, 0x0b)
	}

	code := []byte{0x05}
	code = append(code, v4Entry(onEvent)...)
	code = append(code, v4Entry(intConst(0x41, 0x0400))...)
	code = append(code, v4Entry(i64Const(caps))...)
	code = append(code, v4Entry(intConst(0x41, 4096))...)
	code = append(code, v4Entry(intConst(0x41, 49152))...)

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, v4Section(0x01, types)...)
	out = append(out, v4Section(0x03, funcs)...)
	out = append(out, v4Section(0x05, memory)...)
	out = append(out, v4Section(0x07, exports)...)
	out = append(out, v4Section(0x0a, code)...)
	return out
}

// v4Verdict returns a v4 module whose on_event returns verdict.
func v4Verdict(verdict byte) []byte {
	return v4Module([]byte{0x00, 0x42, verdict, 0x0b})
}

// v4Infinite returns a v4 module whose on_event loops forever, used to prove
// the per-call timeout interrupts a hung guest.
func v4Infinite() []byte {
	return v4Module([]byte{0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x42, 0x00, 0x0b})
}

// v4DenyX inspects the first byte of the envelope blob and denies when it is
// 'X'. It is used by once/stream parity tests.
func v4DenyX() []byte {
	body := []byte{
		0x00,
		0x20, 0x03, // local.get 3 (envelope ptr)
		0x28, 0x02, 0x1c, // i32.load offset=28 (blob section offset)
		0x20, 0x03, // local.get 3 (window base)
		0x6a,             // i32.add -> absolute blob address
		0x2d, 0x00, 0x00, // i32.load8_u offset=0
		0x41, 0xd8, 0x00, // i32.const 'X'
		0x46, // i32.eq
		0xad, // i64.extend_i32_u
		0x0b, // end
	}
	return v4Module(body)
}
