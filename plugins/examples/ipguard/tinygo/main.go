//go:build tinygo || wasm

// Example WasmGate plugin in Go (TinyGo / Go 1.24+ go:wasmexport).
//
// Build with TinyGo:
//
//	tinygo build -o ipguard.wasm -target=wasm-unknown -no-debug ./tinygo
package main

//go:wasmexport on_accept
func onAccept(ipHi int64, ipLo int64, port int32) int32 {
	_ = ipHi
	_ = port
	// Deny any IPv4 address whose last octet is 99.
	if (ipLo & 0xff) == 99 {
		return 1
	}
	return 0
}

//go:wasmexport on_close
func onClose(ipHi int64, ipLo int64, rxBytes int64, txBytes int64) {
	_, _, _, _ = ipHi, ipLo, rxBytes, txBytes
}

func main() {}
