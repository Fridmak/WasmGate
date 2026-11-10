//! `http_guard` — stateless HTTP/1.x policy in one WASM plugin.
//!
//! It demonstrates the full `on_data_ex` ABI:
//!   * `dir == 0` (client -> server): request method, path, Host, User-Agent,
//!     duplicate Content-Length and CL+TE smuggling checks, optional Basic auth;
//!   * `dir == 1` (server -> client): in-place masking of `Server` and
//!     `X-Powered-By` response headers;
//!   * both directions are reached only in `data_mode: "stream"` (in `once`
//!     mode the proxy inspects the first client packet and restores splice).
//!
//! Run it with `data_mode: "stream"`:
//!
//! ```yaml
//! plugin:
//!   path: "plugins/examples/http_guard/http_guard.wasm"
//!   data_mode: "stream"
//! ```
//!
//! Limits (shared by every stateless plugin):
//!   * at most 32 KiB per call, no stream reassembly;
//!   * a header block split across TCP reads is only partly visible;
//!   * a denial closes the connection; the guest cannot synthesise HTTP 403.
//! These are exactly the limits that per-connection state (ABI v3) would fix.

#![no_std]

use core::panic::PanicInfo;
use wg_common as wg;

#[panic_handler]
fn panic(_: &PanicInfo) -> ! {
    core::arch::wasm32::unreachable()
}

/// Methods accepted by the policy. TRACE/CONNECT are deliberately absent.
const ALLOWED_METHODS: &[&[u8]] = &[
    b"GET",
    b"HEAD",
    b"POST",
    b"PUT",
    b"PATCH",
    b"DELETE",
    b"OPTIONS",
];

/// Path prefixes blocked before the request reaches the upstream.
const BLOCKED_PREFIXES: &[&[u8]] = &[
    b"/admin",
    b"/.git",
    b"/.env",
    b"/wp-login",
    b"/wp-admin",
    b"/phpmyadmin",
    b"/actuator",
    b"/server-status",
];

/// User-Agent substrings blocked case-insensitively.
const BLOCKED_UA: &[&[u8]] = &[
    b"sqlmap",
    b"nikto",
    b"nmap",
    b"masscan",
    b"zgrab",
    b"nuclei",
];

/// Set to `None` to disable Basic auth, or change the expected value.
/// Default credential: user / pass (`dXNlcjpwYXNz`).
const REQUIRED_AUTHORIZATION: Option<&[u8]> = Some(b"Basic dXNlcjpwYXNz");

const MAX_HEADER_LINES: usize = 100;
const MAX_HEADER_LINE: usize = 8 * 1024;
const MAX_TARGET: usize = 8 * 1024;

#[no_mangle]
pub extern "C" fn on_data_ptr() -> i32 {
    wg::scratch_ptr()
}

#[no_mangle]
pub extern "C" fn on_data_ex(_conn_id: i64, dir: i32, ptr: i32, len: i32) -> i32 {
    let Some(buf) = (unsafe { wg::payload_mut(ptr, len) }) else {
        return 1;
    };

    match dir {
        0 => {
            if decide_request(buf) {
                0
            } else {
                1
            }
        }
        1 => {
            filter_response(buf);
            0
        }
        _ => 1,
    }
}

/// Applies the request policy. Non-HTTP payloads are passed through: this
/// plugin adds HTTP policy, it is not a protocol detector.
fn decide_request(payload: &[u8]) -> bool {
    if !wg::looks_like_http(payload) {
        return true;
    }
    let Some(req) = wg::parse_request(payload) else {
        // Partial first line: a stateless plugin cannot distinguish this from
        // a segmented request. Fail open; see the README.
        return true;
    };

    if !method_allowed(req.method) {
        return false;
    }
    if bad_target(req.target) {
        return false;
    }
    if req.version == b"HTTP/1.1" && wg::header_value(req.headers, b"Host").is_none() {
        return false;
    }
    if !headers_sane(req.headers) {
        return false;
    }

    // Request smuggling: duplicate Content-Length, or both CL and TE.
    let cl = wg::header_count(req.headers, b"Content-Length");
    let te = wg::header_count(req.headers, b"Transfer-Encoding");
    if cl > 1 || (cl > 0 && te > 0) {
        return false;
    }

    if let Some(ua) = wg::header_value(req.headers, b"User-Agent") {
        for bad in BLOCKED_UA {
            if wg::contains_ci(ua, bad) {
                return false;
            }
        }
    }

    if let Some(host) = wg::header_value(req.headers, b"Host") {
        if host.is_empty() || wg::contains(host, b"..") || host.len() > 255 {
            return false;
        }
    }

    if let Some(expected) = REQUIRED_AUTHORIZATION {
        match wg::header_value(req.headers, b"Authorization") {
            Some(v) if wg::eq_ci(v, expected) => {}
            _ => return false,
        }
    }

    true
}

fn method_allowed(method: &[u8]) -> bool {
    ALLOWED_METHODS.iter().any(|m| *m == method)
}

fn bad_target(target: &[u8]) -> bool {
    if target.len() > MAX_TARGET {
        return true;
    }
    if wg::contains(target, b"../")
        || wg::contains(target, b"..\\")
        || wg::contains_ci(target, b"%2e%2e")
        || wg::contains_ci(target, b"%2f..")
        || wg::contains_ci(target, b"%5c..")
        || wg::contains(target, b"//")
    {
        return true;
    }
    BLOCKED_PREFIXES
        .iter()
        .any(|p| wg::starts_with_ci(target, p))
}

fn headers_sane(headers: &[u8]) -> bool {
    let mut count = 0;
    for line in headers.split(|&b| b == b'\n') {
        count += 1;
        if count > MAX_HEADER_LINES || line.len() > MAX_HEADER_LINE {
            return false;
        }
        // Reject control bytes other than HTAB, CR and LF.
        for &c in line {
            if c < 0x20 && c != b'\t' && c != b'\r' {
                return false;
            }
        }
    }
    true
}

/// Masks infrastructure headers in place. The replacement is padded with
/// spaces to the original value length, because the host cannot resize a chunk.
fn filter_response(payload: &mut [u8]) {
    if wg::parse_status(payload).is_none() {
        return;
    }
    let _ = wg::mask_header(payload, b"Server", b"wasmgate");
    let _ = wg::mask_header(payload, b"X-Powered-By", b"hidden");
}
