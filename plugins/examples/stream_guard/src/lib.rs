//! `stream_guard` — bidirectional body inspection and in-place response
//! scrubbing, the nginx `sub_filter` + WAF-style body scan pattern.
//!
//! ```yaml
//! plugin:
//!   path: "plugins/examples/stream_guard/stream_guard.wasm"
//!   data_mode: "stream"
//! ```
//!
//! What it shows that the header-only examples cannot:
//!   * `dir == 0`: scans the *body* bytes (not just headers) for SQLi, XSS,
//!     traversal and Log4Shell signatures and closes the connection on a hit;
//!   * `dir == 1`: rewrites JSON secrets in the response body in place, e.g.
//!     `"password":"hunter2"` -> `"password":"*******"`. Length is preserved
//!     because the host cannot resize a chunk;
//!   * every chunk is inspected independently, so a signature split across two
//!     TCP reads is missed. Full body coverage needs ABI v3 state.
//!
//! Mutation is copied back by the host for both directions, so the scrubbed
//! response is what the client actually receives.

#![no_std]

use core::panic::PanicInfo;
use wg_common as wg;

#[panic_handler]
fn panic(_: &PanicInfo) -> ! {
    core::arch::wasm32::unreachable()
}

/// Attack signatures checked case-insensitively on every client chunk.
const REQUEST_SIGNATURES: &[&[u8]] = &[
    b"union select",
    b"union all select",
    b"or 1=1",
    b"' or '",
    b"<script",
    b"javascript:",
    b"onerror=",
    b"onload=",
    b"../../",
    b"..%2f",
    b"%2e%2e%2f",
    b"${jndi:",
    b"/etc/passwd",
    b";cat ",
    b"benchmark(",
    b"sleep(",
    b"pg_sleep(",
    b"information_schema",
];

/// JSON keys whose values are masked in server responses.
const SECRET_KEYS: &[&[u8]] = &[
    b"password",
    b"passwd",
    b"secret",
    b"api_key",
    b"apikey",
    b"access_token",
    b"refresh_token",
    b"id_token",
    b"session_id",
    b"private_key",
];

const MASK: u8 = b'*';

#[no_mangle]
pub extern "C" fn on_data_ptr() -> i32 {
    wg::scratch_ptr()
}

#[no_mangle]
pub extern "C" fn on_data_ex(_conn_id: i64, dir: i32, ptr: i32, len: i32) -> i32 {
    match dir {
        0 => {
            let Some(payload) = (unsafe { wg::payload(ptr, len) }) else {
                return 1;
            };
            if has_attack_signature(payload) {
                1
            } else {
                0
            }
        }
        1 => {
            let Some(payload) = (unsafe { wg::payload_mut(ptr, len) }) else {
                return 1;
            };
            scrub(payload);
            0
        }
        _ => 1,
    }
}

fn has_attack_signature(payload: &[u8]) -> bool {
    REQUEST_SIGNATURES
        .iter()
        .any(|sig| wg::contains_ci(payload, sig))
}

/// Masks JSON string values for sensitive keys and hides common banner headers.
/// All writes are in place, so the chunk length never changes.
fn scrub(payload: &mut [u8]) {
    for key in SECRET_KEYS {
        mask_json_value(payload, key);
    }
    let _ = wg::mask_header(payload, b"Server", b"wasmgate");
    let _ = wg::mask_header(payload, b"X-Powered-By", b"hidden");
    let _ = wg::mask_header(payload, b"X-AspNet-Version", b"hidden");
}

/// Finds `"key"` followed by `:` and a JSON string, and replaces the string
/// body with `*`. Values are found anywhere in the chunk, including inside a
/// body that was split from its response headers.
fn mask_json_value(buf: &mut [u8], key: &[u8]) -> bool {
    let mut from = 0;
    while from < buf.len() {
        let quote = match wg::find(&buf[from..], b"\"") {
            Some(off) => from + off,
            None => return false,
        };
        let name_start = quote + 1;
        let name_end = match wg::find(&buf[name_start..], b"\"") {
            Some(off) => name_start + off,
            None => return false,
        };
        if wg::eq_ci(&buf[name_start..name_end], key) {
            if mask_after_key(buf, name_end + 1) {
                return true;
            }
        }
        from = name_end + 1;
    }
    false
}

fn mask_after_key(buf: &mut [u8], mut i: usize) -> bool {
    // Skip optional whitespace and the colon.
    while let Some(&c) = buf.get(i) {
        if c == b' ' || c == b'\t' || c == b'\r' || c == b'\n' {
            i += 1;
        } else {
            break;
        }
    }
    if buf.get(i) != Some(&b':') {
        return false;
    }
    i += 1;
    while let Some(&c) = buf.get(i) {
        if c == b' ' || c == b'\t' || c == b'\r' || c == b'\n' {
            i += 1;
        } else {
            break;
        }
    }
    if buf.get(i) != Some(&b'"') {
        return false;
    }
    i += 1;
    let start = i;
    while let Some(&c) = buf.get(i) {
        if c == b'"' || c == b'\n' {
            break;
        }
        buf[i] = MASK;
        i += 1;
    }
    i > start
}
