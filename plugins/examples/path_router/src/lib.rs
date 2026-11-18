//! `path_router` — L7 routing by HTTP Host and path in `data_mode: headers`.
//!
//! This is the nginx `map`/`location -> upstream` pattern, moved into a WASM
//! plugin. The opening request head is inspected before the upstream is dialed;
//! the verdict selects a pool from `plugin.route_pools`:
//!
//! ```yaml
//! plugin:
//!   path: "plugins/examples/path_router/path_router.wasm"
//!   data_mode: "headers"
//!   timeout: 200ms
//!   route_pools: ["api", "static", "ws"]
//! ```
//!
//! verdict 0 = default pool, 1 = deny, 2 = route_pools[0], 3 = route_pools[1],
//! 4 = route_pools[2]. The selected upstream is fixed for this connection.

#![no_std]

use wg_common::{self as wg, v4};

wg::wg_v4_plugin!(v4::CAP_HTTP_HEADERS, inspect);

const VERDICT_DEFAULT: i32 = 0;
const VERDICT_DENY: i32 = 1;
const ROUTE_API: i32 = 2; // route_pools[0]
const ROUTE_STATIC: i32 = 3; // route_pools[1]
const ROUTE_WS: i32 = 4; // route_pools[2]

fn inspect(call: v4::Call, event: v4::Event<'_>, _: &mut v4::Output<'_>) -> i32 {
    if call.phase != v4::Phase::RequestHeaders {
        return VERDICT_DENY;
    }
    let Some(req) = event.request_line() else {
        return VERDICT_DENY;
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
    if let Some(host) = event.headers().first(b"Host") {
        let host = strip_port(wg::trim_ascii(host));
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
    if wg::starts_with_ci(req.target, b"/static/") || wg::starts_with_ci(req.target, b"/assets/") {
        return ROUTE_STATIC;
    }
    if wg::starts_with_ci(req.target, b"/ws") || wg::starts_with_ci(req.target, b"/socket") {
        return ROUTE_WS;
    }

    VERDICT_DEFAULT
}

/// `example.com:8080` → `example.com` and `[2001:db8::1]:8080` → `[2001:db8::1]`,
/// without allocating.
fn strip_port(host: &[u8]) -> &[u8] {
    if host.first() == Some(&b'[') {
        if let Some(end) = host.iter().position(|&b| b == b']') {
            return &host[..=end];
        }
    }
    match host.iter().rposition(|&b| b == b':') {
        Some(i) => &host[..i],
        None => host,
    }
}
