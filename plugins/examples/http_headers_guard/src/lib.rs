//! `http_headers_guard` — request-header policy for `data_mode: headers`.
//!
//! The host calls `on_headers` exactly once with a complete HTTP/1.x request
//! header block, then splices the body. This is a connection-level policy hook:
//! it protects the first request of a connection, not every keep-alive request,
//! and it does not inspect bodies or responses. Use `data_mode: stream` when
//! those guarantees are required.
//!
//! The example blocks unsafe methods, sensitive paths, malformed headers,
//! duplicate `Content-Length` and `Content-Length` + `Transfer-Encoding`
//! smuggling attempts. It is intentionally small and dependency-free.

#![no_std]

use core::panic::PanicInfo;
use wg_common as wg;

#[panic_handler]
fn panic(_: &PanicInfo) -> ! {
    core::arch::wasm32::unreachable()
}

const ALLOWED_METHODS: &[&[u8]] = &[
    b"GET", b"HEAD", b"POST", b"PUT", b"PATCH", b"DELETE", b"OPTIONS",
];

const BLOCKED_PREFIXES: &[&[u8]] = &[b"/admin", b"/.git", b"/.env", b"/actuator"];

/// Returns true when the request target is malformed, absolute-form, escapes
/// the path root, or normalizes to a blocked prefix. Backends commonly
/// normalize dot-segments and percent-encoding, so a prefix check on the raw
/// target is not enough.
fn target_blocked(target: &[u8]) -> bool {
    let mut decoded = [0u8; 2048];
    let mut dlen = 0usize;
    let mut i = 0usize;
    while i < target.len() {
        let mut b = target[i];
        if b == b'?' || b == b'#' {
            break;
        }
        if b == b'%' {
            let Some(&hi) = target.get(i + 1) else {
                return true;
            };
            let Some(&lo) = target.get(i + 2) else {
                return true;
            };
            let Some(h) = hex(hi) else { return true };
            let Some(l) = hex(lo) else { return true };
            b = (h << 4) | l;
            i += 3;
        } else {
            i += 1;
        }
        if b == b'\\' {
            b = b'/';
        }
        if dlen >= decoded.len() {
            return true;
        }
        decoded[dlen] = b;
        dlen += 1;
    }
    if dlen == 0 || decoded[0] != b'/' {
        return true; // origin-form only; absolute-form and '*' are rejected
    }

    let mut out = [0u8; 2048];
    let mut olen = 0usize;
    let mut j = 1usize;
    while j <= dlen {
        let start = j;
        while j < dlen && decoded[j] != b'/' {
            j += 1;
        }
        let segment = &decoded[start..j];
        if segment == b".." {
            if olen == 0 {
                return true;
            }
            while olen > 0 && out[olen - 1] != b'/' {
                olen -= 1;
            }
            if olen > 0 {
                olen -= 1;
            }
        } else if !segment.is_empty() && segment != b"." {
            if olen + 1 + segment.len() > out.len() {
                return true;
            }
            out[olen] = b'/';
            olen += 1;
            let mut k = 0usize;
            while k < segment.len() {
                out[olen] = segment[k];
                olen += 1;
                k += 1;
            }
        }
        if j < dlen {
            j += 1;
        } else {
            break;
        }
    }
    if olen == 0 {
        out[0] = b'/';
        olen = 1;
    }

    BLOCKED_PREFIXES
        .iter()
        .any(|prefix| olen >= prefix.len() && wg::eq_ci(&out[..prefix.len()], prefix))
}

fn hex(c: u8) -> Option<u8> {
    match c {
        b'0'..=b'9' => Some(c - b'0'),
        b'a'..=b'f' => Some(c - b'a' + 10),
        b'A'..=b'F' => Some(c - b'A' + 10),
        _ => None,
    }
}

#[no_mangle]
pub extern "C" fn on_data_ptr() -> i32 {
    wg::scratch_ptr()
}

#[no_mangle]
pub extern "C" fn on_headers(_conn_id: i64, ptr: i32, len: i32) -> i32 {
    let Some(buf) = (unsafe { wg::payload(ptr, len) }) else {
        return 1;
    };
    let Some(req) = wg::parse_request_headers(buf) else {
        return 1;
    };

    if !ALLOWED_METHODS.iter().any(|m| *m == req.method) {
        return 1;
    }
    if target_blocked(req.target) {
        return 1;
    }
    if req.version == b"HTTP/1.1" && wg::header_value(req.headers, b"Host").is_none() {
        return 1;
    }

    let content_length = wg::header_count(req.headers, b"Content-Length");
    let transfer_encoding = wg::header_count(req.headers, b"Transfer-Encoding");
    if content_length > 1 || (content_length > 0 && transfer_encoding > 0) {
        return 1;
    }

    0
}
