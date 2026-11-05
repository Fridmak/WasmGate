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
pub extern "C" fn on_accept(_ip_hi: i64, ip_lo: i64, _port: i32) -> i32 {
    // Reject any IPv4 host ending in .99 (e.g. 192.0.2.99 or 127.0.0.99).
    if (ip_lo & 0xff) == 99 {
        return 1;
    }
    0
}

#[no_mangle]
pub extern "C" fn on_close(_ip_hi: i64, _ip_lo: i64, _rx_bytes: i64, _tx_bytes: i64) {}

// Optional L7 payload inspection hook:
//   ptr, len — slice inside the plugin's linear memory containing the packet bytes
//   return   — 0 (VerdictAllow) or 1 (VerdictDeny)
//
// Uncomment to enable L7 inspection ("once" mode inspects the first request
// packet and then restores Linux splice zero-copy; "stream" mode inspects every chunk):
//
// #[no_mangle]
// pub unsafe extern "C" fn on_data(ptr: *const u8, len: usize) -> i32 {
//     let payload = core::slice::from_raw_parts(ptr, len);
//     if !payload.is_empty() && payload[0] == b'X' {
//         return 1;
//     }
//     0
// }

