//! `path_router` — L7 routing by HTTP Host and path in `data_mode: "once"`.
//!
//! This is the nginx `map`/`location -> upstream` pattern, moved into a WASM
//! plugin. The first client packet is inspected before the upstream is dialed;
//! the verdict selects a pool from `plugin.route_pools`:
//!
//! ```yaml
//! plugin:
//!   path: "plugins/examples/path_router/path_router.wasm"
//!   data_mode: "once"
//!   route_pools: ["api", "static", "ws"]
//! ```
//!
//! verdict 0 = default pool, 1 = deny, 2 = route_pools[0], 3 = route_pools[1],
//! 4 = route_pools[2]. After the verdict the proxy replays the peeked packet
//! with `DrainTo` and the rest of the connection stays on `splice(2)`.
//!
//! Because this is the legacy `on_data(ptr, len)` hook it only sees the
//! client -> server direction. For response routing use `on_data_ex`.

#![no_std]

use core::panic::PanicInfo;
use wg_common as wg;

#[panic_handler]
fn panic(_: &PanicInfo) -> ! {
    core::arch::wasm32::unreachable()
}

const VERDICT_DEFAULT: i32 = 0;
const VERDICT_DENY: i32 = 1;
const ROUTE_API: i32 = 2; // route_pools[0]
const ROUTE_STATIC: i32 = 3; // route_pools[1]
const ROUTE_WS: i32 = 4; // route_pools[2]

#[no_mangle]
pub extern "C" fn on_data_ptr() -> i32 {
    wg::scratch_ptr()
}

#[no_mangle]
pub extern "C" fn on_data(ptr: i32, len: i32) -> i32 {
    let Some(payload) = (unsafe { wg::payload(ptr, len) }) else {
        return VERDICT_DENY;
    };
    if !wg::looks_like_http(payload) {
        return VERDICT_DEFAULT;
    }
    let Some(req) = wg::parse_request(payload) else {
        // Incomplete first line: route to the default pool, never guess.
        return VERDICT_DEFAULT;
    };

    if wg::starts_with_ci(req.target, b"/health") {
        return VERDICT_DEFAULT;
    }
    if wg::starts_with_ci(req.target, b"/admin")
        || wg::contains(req.target, b"../")
        || wg::contains_ci(req.target, b"%2e%2e")
    {
        return VERDICT_DENY;
    }

    // Host-based routing takes precedence, then the path prefix.
    if let Some(host) = wg::header_value(req.headers, b"Host") {
        let host = strip_port(host);
        if wg::starts_with_ci(host, b"api.") {
            return ROUTE_API;
        }
        if wg::starts_with_ci(host, b"static.") {
            return ROUTE_STATIC;
        }
        if wg::starts_with_ci(host, b"ws.") || wg::starts_with_ci(host, b"socket.") {
            return ROUTE_WS;
        }
    }

    if wg::starts_with_ci(req.target, b"/api/") || wg::eq_ci(req.target, b"/api") {
        return ROUTE_API;
    }
    if wg::starts_with_ci(req.target, b"/static/")
        || wg::starts_with_ci(req.target, b"/assets/")
    {
        return ROUTE_STATIC;
    }
    if wg::starts_with_ci(req.target, b"/ws")
        || wg::starts_with_ci(req.target, b"/socket")
    {
        return ROUTE_WS;
    }

    VERDICT_DEFAULT
}

/// `example.com:8080` -> `example.com`, without allocating.
fn strip_port(host: &[u8]) -> &[u8] {
    match host.iter().position(|&b| b == b':') {
        Some(i) => &host[..i],
        None => host,
    }
}
