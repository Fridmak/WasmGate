package proxy

import "wasmgate/internal/plugin/abi4"

// Hand-assembled ABI v4 L4 lifecycle module for proxy tests. It exports
// on_accept/on_close plus wg_abi/wg_caps and no memory or on_event.

func v4Lifecycle(accept byte) []byte {
	types := []byte{
		0x04,
		0x60, 0x03, 0x7e, 0x7e, 0x7f, 0x01, 0x7f, // on_accept
		0x60, 0x00, 0x01, 0x7f, // wg_abi
		0x60, 0x00, 0x01, 0x7e, // wg_caps
		0x60, 0x04, 0x7e, 0x7e, 0x7e, 0x7e, 0x00, // on_close
	}
	funcs := []byte{0x04, 0x00, 0x01, 0x02, 0x03}
	exports := []byte{0x04}
	addExport := func(name string, idx uint32) {
		exports = append(exports, v4ULEB(uint32(len(name)))...)
		exports = append(exports, name...)
		exports = append(exports, 0x00)
		exports = append(exports, v4ULEB(idx)...)
	}
	addExport("on_accept", 0)
	addExport("wg_abi", 1)
	addExport("wg_caps", 2)
	addExport("on_close", 3)

	code := make([]byte, 1, 32)
	code[0] = 4
	code = append(code, v4Entry([]byte{0x00, 0x41, accept, 0x0b})...)
	abi := make([]byte, 0, 6)
	abi = append(abi, 0x00, 0x41)
	abi = append(abi, v4ULEB(0x0400)...)
	abi = append(abi, 0x0b)
	code = append(code, v4Entry(abi)...)
	caps := append([]byte{0x00, 0x42}, v4SLEB(int32(abi4.CapLifecycle))...)
	code = append(code, v4Entry(append(caps, 0x0b))...)
	code = append(code, v4Entry([]byte{0x00, 0x0b})...)

	out := make([]byte, 0, len(code)+len(exports)+64)
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)
	out = append(out, v4Section(0x01, types)...)
	out = append(out, v4Section(0x03, funcs)...)
	out = append(out, v4Section(0x07, exports)...)
	out = append(out, v4Section(0x0a, code)...)
	return out
}
