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
    if BLOCKED_PREFIXES
        .iter()
        .any(|prefix| wg::starts_with_ci(req.target, prefix))
    {
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
