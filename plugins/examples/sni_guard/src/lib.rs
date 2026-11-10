//! `sni_guard` — TLS ClientHello inspection without terminating TLS.
//!
//! This is the nginx `ssl_preread` + `map` pattern: the proxy peeks the
//! ClientHello, the plugin parses the SNI extension and returns a route. The
//! connection stays end-to-end encrypted and, in `once` mode, returns to
//! `splice(2)` after the first packet.
//!
//! ```yaml
//! plugin:
//!   path: "plugins/examples/sni_guard/sni_guard.wasm"
//!   data_mode: "once"
//!   route_pools: ["api", "static", "internal"]
//! sni_routes:                # non-empty forces a full ClientHello peek
//!   "api.example.com": web
//! ```
//!
//! The plugin sees ciphertext, so it can only read the ClientHello record; it
//! cannot inspect HTTP inside the tunnel. Enable `listeners[].tls` for that.
//!
//! Verdicts: 0 = default pool, 1 = deny, 2/3/4 = route_pools[0..2].

#![no_std]

use core::panic::PanicInfo;
use wg_common as wg;

#[panic_handler]
fn panic(_: &PanicInfo) -> ! {
    core::arch::wasm32::unreachable()
}

const VERDICT_DEFAULT: i32 = 0;
const VERDICT_DENY: i32 = 1;
const ROUTE_API: i32 = 2;
const ROUTE_STATIC: i32 = 3;
const ROUTE_INTERNAL: i32 = 4;

/// Exact host or any subdomain of it.
const BLOCKED_DOMAINS: &[&[u8]] = &[b"blocked.example.com", b"malware.test"];

#[no_mangle]
pub extern "C" fn on_data_ptr() -> i32 {
    wg::scratch_ptr()
}

#[no_mangle]
pub extern "C" fn on_data(ptr: i32, len: i32) -> i32 {
    let Some(payload) = (unsafe { wg::payload(ptr, len) }) else {
        return VERDICT_DENY;
    };
    let Some(sni) = wg::parse_sni(payload) else {
        // Not a complete ClientHello: leave policy to the default pool. The
        // proxy only guarantees a full record when `sni_routes` is non-empty.
        return VERDICT_DEFAULT;
    };

    if BLOCKED_DOMAINS.iter().any(|d| host_in_domain(sni, d)) {
        return VERDICT_DENY;
    }

    if host_in_domain(sni, b"api.example.com") {
        return ROUTE_API;
    }
    if host_in_domain(sni, b"static.example.com") {
        return ROUTE_STATIC;
    }
    if host_in_domain(sni, b"internal.example.com") {
        return ROUTE_INTERNAL;
    }

    VERDICT_DEFAULT
}

/// True for `example.com` and `*.example.com`, case-insensitively.
fn host_in_domain(host: &[u8], domain: &[u8]) -> bool {
    if wg::eq_ci(host, domain) {
        return true;
    }
    host.len() > domain.len()
        && host[host.len() - domain.len() - 1] == b'.'
        && wg::ends_with_ci(host, domain)
}
