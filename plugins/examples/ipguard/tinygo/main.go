//go:build tinygo || wasm

// Example WasmGate plugin in Go (TinyGo / Go 1.24+ go:wasmexport).
//
// Build with TinyGo:
//
//	tinygo build -o ipguard.wasm -target=wasm-unknown -no-debug ./tinygo
package main

//go:wasmexport on_accept
func onAccept(ipHi int64, ipLo int64, port int32) int32 {
	_ = port
	// IPv4 is represented as ::ffff:a.b.c.d: high word zero, upper 32 bits of
	// the low word 0x0000ffff. Deny any IPv4 address whose last octet is 99.
	const ipv4MappedMask = int64(0x0000ffff)
	if ipHi == 0 && (ipLo>>32) == ipv4MappedMask && (ipLo&0xff) == 99 {
		return 1
	}
	return 0
}

//go:wasmexport on_close
func onClose(ipHi int64, ipLo int64, rxBytes int64, txBytes int64) {
	_, _, _, _ = ipHi, ipLo, rxBytes, txBytes
}

func main() {}
