// Example WasmGate plugin in Rust (#![no_std], zero dependencies).
//
// Build:
//   rustup target add wasm32-unknown-unknown
//   cargo build --release --target wasm32-unknown-unknown
//   cp target/wasm32-unknown-unknown/release/ipguard.wasm ./ipguard.wasm
#![no_std]

use core::panic::PanicInfo;

#[panic_handler]
fn panic(_info: &PanicInfo) -> ! {
    core::arch::wasm32::unreachable()
}

// Host ABI:
//   ip_hi, ip_lo — canonical 16-byte IP in big-endian (IPv4 is IPv4-mapped IPv6 ::ffff:a.b.c.d)
//   port         — client source TCP port
//   return       — 0 (VerdictAllow) or 1 (VerdictDeny)
#[no_mangle]
pub extern "C" fn on_accept(ip_hi: i64, ip_lo: i64, _port: i32) -> i32 {
    // IPv4 is represented as ::ffff:a.b.c.d: the high word is zero and the
    // upper 32 bits of the low word are 0x0000ffff.
    let ipv4_mapped = ip_hi == 0 && (ip_lo >> 32) == 0x0000_ffff;
    if ipv4_mapped && (ip_lo & 0xff) == 99 {
        return 1;
    }
    0
}

#[no_mangle]
pub extern "C" fn on_close(_ip_hi: i64, _ip_lo: i64, _rx_bytes: i64, _tx_bytes: i64) {}

// This example intentionally exports only the L4 hooks. For L7 inspection see
// http_guard, stream_guard and http_body_guard.
